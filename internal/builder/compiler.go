package builder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// secretEnvNames are server secrets that must never be inherited by a build
// subprocess (bun install / astro build run tenant-influenced JS and dependency
// scripts). Kept alongside a suffix heuristic so a future secret-shaped var is
// stripped by default.
var secretEnvNames = map[string]bool{
	"ADMIN_PASSWORD": true, "JWT_SECRET": true, "VAULT_KEY": true,
	"ATOMICSITE_SHIELD_KEY": true, "ANALYTICS_SALT": true,
	"MOLLIE_API_KEY": true, "MAILERSEND_API_TOKEN": true,
	"ATOMICSITE_CLOUDFLARE_TOKEN": true, "CLOUDFLARE_TOKEN": true,
	"SESSION_SECRET": true, "DATABASE_URL": true, "DB_PATH": true,
}

func isSecretEnvName(name string) bool {
	if secretEnvNames[name] {
		return true
	}
	u := strings.ToUpper(name)
	for _, suffix := range []string{"_SECRET", "_TOKEN", "_API_KEY", "_APIKEY", "_PASSWORD", "_PASS", "_PRIVATE_KEY", "_CREDENTIAL", "_CREDENTIALS"} {
		if strings.HasSuffix(u, suffix) {
			return true
		}
	}
	return false
}

// buildAllowedEnvNames is the exact set of variables a build subprocess gets.
//
// This used to be a denylist (a name list plus a secret-shaped suffix
// heuristic), and it lost. LITESTREAM_SECRET_ACCESS_KEY ends in _ACCESS_KEY,
// which is not one of the checked suffixes (_API_KEY and _APIKEY are),
// LITESTREAM_ACCESS_KEY_ID ends in _ID, and both are set on the server process
// by docker-compose.yml:43-44. So the credentials for the object-storage
// replica of the entire multi-tenant SQLite database were readable by any code
// running in a tenant's build. BRIGHTCRM_WEBHOOK_SECRET_PREVIOUS (_PREVIOUS),
// FLARE_DSN and ATOMICSITE_SHIELD_REDIS_URL leaked the same way.
//
// A denylist over an environment that grows cannot be made correct: every new
// variable is exposed until someone remembers to name it. An allowlist is
// wrong in the safe direction, and a build that needs another variable fails
// loudly instead of leaking silently.
var buildAllowedEnvNames = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"TMPDIR": true, "TMP": true, "TEMP": true,
	"LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TZ": true,
	"TERM": true, "CI": true,
	"XDG_CACHE_HOME": true, "XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true,
	"BUN_INSTALL": true, "BUN_INSTALL_CACHE_DIR": true,
	"npm_config_cache": true,
	"ASTRO_TELEMETRY_DISABLED": true, "DO_NOT_TRACK": true,
}

// buildAllowedEnvPrefixes covers the toolchain families where the exact names
// are not fixed. Keep these narrow: a broad prefix reopens the hole.
var buildAllowedEnvPrefixes = []string{"NODE_", "BUN_"}

// buildEnv returns the environment a build subprocess may see. `bun install`
// and `astro build` execute tenant-influenced JavaScript (component templates,
// dependency install scripts), so anything left in here is readable by a
// tenant.
func buildEnv() []string {
	src := os.Environ()
	env := make([]string, 0, 16)
	for _, kv := range src {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if buildEnvAllowed(name) {
			env = append(env, kv)
		}
	}
	return env
}

func buildEnvAllowed(name string) bool {
	if buildAllowedEnvNames[name] {
		return true
	}
	for _, p := range buildAllowedEnvPrefixes {
		if strings.HasPrefix(name, p) {
			// Belt and braces: a NODE_/BUN_ variable that is itself
			// secret-shaped (someone's NODE_AUTH_TOKEN) still gets dropped.
			return !isSecretEnvName(name)
		}
	}
	return false
}

// BuildTimeout caps a single end-to-end Compile() (bun install + bun run
// build). 10 minutes is generous for a fresh install on a multi-page site
// with image processing and tight enough that a deadlocked bun process
// doesn't pin a goroutine forever (audit finding H4).
//
// The timeout is enforced via exec.CommandContext: when the deadline hits,
// the bun process gets a SIGKILL and the goroutine returns with an error.
// Without this, a single hung bun could pile up unbounded goroutines and
// memory across repeated build retries.
const BuildTimeout = 10 * time.Minute

// MaxBuildLogBytes caps the captured build log so a particularly verbose
// bun error doesn't blow up memory or the deployments.build_log column
// (audit finding L2). 1 MB covers any plausible Astro / Vite stack trace
// while bounding the worst case.
const MaxBuildLogBytes = 1024 * 1024

// CompileResult holds the output of a build.
type CompileResult struct {
	DistDir    string
	BuildLog   string
	DurationMs int64
	Success    bool
	Error      string
}

// killWaitDelay bounds how long Wait blocks on lingering output pipes
// after the process (group) was killed. Without it, an orphaned
// grandchild (vite/esbuild worker) holding the pipe would block the
// build goroutine forever and permanently pin its global build slot.
const killWaitDelay = 10 * time.Second

// cappedBuffer keeps the first max bytes and silently drains the rest,
// so the log cap actually bounds build memory (CombinedOutput used to
// buffer the entire output before capping) while the subprocess never
// blocks on a full pipe.
type cappedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if remaining := c.max - c.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			c.buf.Write(p[:remaining])
			c.truncated = true
		} else {
			c.buf.Write(p)
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	return len(p), nil
}

func (c *cappedBuffer) bytes() []byte {
	if c.truncated {
		return append(c.buf.Bytes(), []byte("\n...[truncated]\n")...)
	}
	return c.buf.Bytes()
}

// runBuildCmd runs a toolchain command with the safety rails every
// build subprocess needs: its own process GROUP (bun spawns astro
// spawns vite/esbuild workers; killing only the direct child leaves
// grandchildren holding the output pipes), a group-wide SIGKILL on ctx
// cancel, a WaitDelay so Wait cannot hang on a leaked pipe, and
// size-capped output.
func runBuildCmd(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = buildEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid = the whole process group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = killWaitDelay
	out := &cappedBuffer{max: MaxBuildLogBytes}
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	return out.bytes(), err
}

// Compile runs bun install + bun run build in the workspace directory.
// The supplied ctx bounds the operation: cancel ctx and the bun child
// processes get killed via exec.CommandContext. If ctx has no deadline
// of its own, we add BuildTimeout so a forgotten context.Background()
// caller still gets the safety net.
func Compile(ctx context.Context, wsDir string) *CompileResult {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, BuildTimeout)
		defer cancel()
	}

	start := time.Now()
	var logBuf bytes.Buffer

	// Step 1: bun install. Try frozen-lockfile first; fall back without
	// when the workspace has no bun.lock yet (first build of a site).
	slog.Info("build: installing dependencies", "workspace", wsDir)
	installOut, err := runBuildCmd(ctx, wsDir, "bun", "install", "--frozen-lockfile")
	appendLogSection(&logBuf, "bun install", installOut)

	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return failureResult(&logBuf, start, fmt.Sprintf("bun install timed out or cancelled: %v", ctxErr))
		}
		installOut, err = runBuildCmd(ctx, wsDir, "bun", "install")
		appendLogSection(&logBuf, "bun install (no lockfile)", installOut)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return failureResult(&logBuf, start, fmt.Sprintf("bun install timed out or cancelled: %v", ctxErr))
			}
			return failureResult(&logBuf, start, fmt.Sprintf("bun install failed: %v", err))
		}
	}

	// Step 2: bun run build. Honour the same deadline.
	slog.Info("build: compiling astro project", "workspace", wsDir)
	buildOut, err := runBuildCmd(ctx, wsDir, "bun", "run", "build")
	appendLogSection(&logBuf, "bun run build", buildOut)

	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return failureResult(&logBuf, start, fmt.Sprintf("astro build timed out or cancelled: %v", ctxErr))
		}
		return failureResult(&logBuf, start, fmt.Sprintf("astro build failed: %v", err))
	}

	distDir := filepath.Join(wsDir, "dist")
	duration := time.Since(start).Milliseconds()

	slog.Info("build: complete", "duration_ms", duration, "dist", distDir)

	return &CompileResult{
		DistDir:    distDir,
		BuildLog:   capLog(logBuf.String()),
		DurationMs: duration,
		Success:    true,
	}
}

// failureResult assembles a CompileResult for the error path. Caps the
// log size; otherwise a misbehaving bun could blow up memory.
func failureResult(logBuf *bytes.Buffer, start time.Time, msg string) *CompileResult {
	return &CompileResult{
		BuildLog:   capLog(logBuf.String()),
		DurationMs: time.Since(start).Milliseconds(),
		Error:      msg,
	}
}

// appendLogSection writes a labelled section to the running buffer.
// Truncates each section's contribution so a runaway log line can't blow
// past MaxBuildLogBytes on its own.
func appendLogSection(buf *bytes.Buffer, label string, out []byte) {
	buf.WriteString("=== ")
	buf.WriteString(label)
	buf.WriteString(" ===\n")
	if len(out) > MaxBuildLogBytes {
		buf.Write(out[:MaxBuildLogBytes])
		buf.WriteString("\n...[truncated]\n")
	} else {
		buf.Write(out)
	}
	buf.WriteString("\n")
}

// capLog clamps the final log string to MaxBuildLogBytes. The truncation
// keeps the head + tail (most useful debugging context) and drops the
// middle when a build produces a multi-MB log.
func capLog(s string) string {
	if len(s) <= MaxBuildLogBytes {
		return s
	}
	half := MaxBuildLogBytes / 2
	return s[:half] + "\n...[truncated middle]...\n" + s[len(s)-half:]
}

// ErrBuildCancelled is returned via CompileResult.Error when the supplied
// context was cancelled (timeout, parent-context kill). Callers can match
// on this prefix to distinguish "build legitimately failed" from "build
// got killed by us".
var ErrBuildCancelled = errors.New("build cancelled")

// PagefindTimeout caps a single pagefind index pass. Sites with 1000+
// pages take a few seconds; 5 minutes is comfortably above the worst
// case we'd want to wait on a build.
const PagefindTimeout = 5 * time.Minute

// RunPagefind generates the static search index for the built site by
// invoking pagefind against dist/. Sprint 5 quick-win (2026-05-24).
// The output lands in dist/pagefind/ and the search_box block's
// runtime loader points at it. Opt-in: callers gate this behind the
// search.pagefind_enabled setting.
//
// pagefind is declared in the workspace skeleton's devDependencies so
// `bun install` in Compile() already fetched it. We invoke via bunx
// to pick up the local copy; no global install required on the
// builder host.
//
// Returns the captured log lines so the renderer can append them to
// the deployment's build_log column, and a single error when pagefind
// itself returns non-zero. Cancellation flows through ctx.
func RunPagefind(ctx context.Context, wsDir string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, PagefindTimeout)
		defer cancel()
	}
	slog.Info("build: running pagefind index", "workspace", wsDir)
	out, err := runBuildCmd(ctx, wsDir, "bunx", "pagefind", "--site", "dist", "--quiet")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return string(out), fmt.Errorf("pagefind timed out or cancelled: %w", ctxErr)
		}
		return string(out), fmt.Errorf("pagefind failed: %w", err)
	}
	return string(out), nil
}
