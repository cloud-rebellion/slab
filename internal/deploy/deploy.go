// Package deploy contains the Deployer abstraction and concrete deployer
// implementations (local copy, rsync over SSH, Dockyard). Each Deployer
// publishes a built site (distDir) to a Target and returns Result stats.
package deploy

import (
	"context"
	"fmt"
	"time"
)

// Result is the outcome of a successful Deploy call.
type Result struct {
	URL        string    `json:"url"`
	DeployedAt time.Time `json:"deployed_at"`
	SizeBytes  int64     `json:"size_bytes"`
	FileCount  int       `json:"file_count"`
}

// Target is the runtime view of a deploy_targets row. Config holds the
// kind-specific JSON config decoded into a map.
type Target struct {
	ID     string
	SiteID string
	// Slug is the site's slug. The wildcard host serves a tenant from
	// /srv/atomicsite/{slug}/dist, so a local target's path legitimately
	// contains the slug (not the site ID). LocalDeployer.Validate accepts the
	// path's tenant segment matching either SiteID or Slug. Optional: when
	// empty, only SiteID is checked.
	Slug   string
	Name   string
	Kind   string
	Config map[string]any
}

// Deployer publishes a built site (distDir) to a Target.
type Deployer interface {
	Kind() string
	Deploy(ctx context.Context, distDir string, target Target) (Result, error)
	Validate(target Target) error
}

// New returns a Deployer for the given kind.
func New(kind string) (Deployer, error) {
	switch kind {
	case "local":
		return &LocalDeployer{}, nil
	case "rsync":
		return &RsyncDeployer{}, nil
	case "dockyard":
		return NewDockyardDeployer(), nil
	}
	return nil, fmt.Errorf("unknown deployer kind: %s", kind)
}
