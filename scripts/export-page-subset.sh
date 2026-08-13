#!/usr/bin/env bash
#
# export-page-subset.sh SRC_DIST OUT_DIR TARGET_DOMAIN SLUG [SLUG...]
#
# Carves a standalone static bundle for ONE tenant out of a built Atomicsite
# dist that hosts several, and repoints every absolute URL at TARGET_DOMAIN.
# The output is drop-in for Netlify, Cloudflare Pages, or any static host that
# reads _headers/_redirects.
#
# Why this exists: the demo instance's site 517e0000 holds the Example Client pages
# alongside ~83 legacy Bright Interaction pages (the whole /insights/ blog,
# /platform/, /sv/*, the founder bio). Handing a client the raw dist would ship
# our own marketing site on their hosting, and every canonical, og:url and
# sitemap entry would still point back at demo.slab.example.com.
#
# The script keeps only the named slugs plus the assets they actually reference,
# then verifies the result: no dangling internal link, no leftover source-domain
# URL, and every referenced asset present. It exits non-zero if any check fails,
# so a broken bundle can never be published silently.
set -euo pipefail

if [ "$#" -lt 4 ]; then
  echo "usage: $0 SRC_DIST OUT_DIR TARGET_DOMAIN SLUG [SLUG...]" >&2
  echo "  TARGET_DOMAIN: hostname only, e.g. example-client.netlify.app" >&2
  exit 2
fi

SRC="${1%/}"; shift
OUT="${1%/}"; shift
DOMAIN="$1"; shift
SLUGS=("$@")

[ -d "$SRC" ] || { echo "source dist not found: $SRC" >&2; exit 1; }
[ -f "$SRC/index.html" ] || { echo "no index.html in $SRC" >&2; exit 1; }

SRC_HOST="$(
  grep -ohE '<link rel="canonical" href="https://[^/"]+' "$SRC/index.html" \
    | head -1 | sed -E 's|.*https://||'
)"
[ -n "$SRC_HOST" ] || { echo "could not read source canonical host from $SRC/index.html" >&2; exit 1; }

echo "source host : $SRC_HOST"
echo "target host : $DOMAIN"
echo "pages       : index ${SLUGS[*]}"

rm -rf "$OUT"; mkdir -p "$OUT"

# ---- 1. pages -------------------------------------------------------------
cp "$SRC/index.html" "$OUT/index.html"
for slug in "${SLUGS[@]}"; do
  [ -f "$SRC/$slug/index.html" ] || { echo "missing page: $slug" >&2; exit 1; }
  mkdir -p "$OUT/$slug"
  cp "$SRC/$slug/index.html" "$OUT/$slug/index.html"
done

# ---- 2. root files worth carrying ------------------------------------------
# Caddyfile is deployment config for the old host; llms.txt and the sitemap are
# regenerated below because both enumerate pages we just dropped.
for f in favicon.ico apple-touch-icon.png humans.txt _headers \
         _atomic-engagement.js _atomic-storefront.js; do
  if [ -f "$SRC/$f" ]; then cp "$SRC/$f" "$OUT/$f"; fi
done
for f in "$SRC"/_ccb.*.js; do
  if [ -f "$f" ]; then cp "$f" "$OUT/"; fi
done
if [ -d "$SRC/.well-known" ]; then cp -R "$SRC/.well-known" "$OUT/.well-known"; fi

# ---- 3. referenced assets only ---------------------------------------------
PAGES=("$OUT/index.html")
for slug in "${SLUGS[@]}"; do PAGES+=("$OUT/$slug/index.html"); done

refs="$(grep -ohE '(href|src|content)="/[^"]*"' "${PAGES[@]}" \
        | sed -E 's/.*="//; s/"$//' | sort -u)"

copy_ref() {
  local rel="${1#/}"
  [ -n "$rel" ] || return 0
  [ -e "$SRC/$rel" ] || return 0
  [ -f "$SRC/$rel" ] || return 0
  mkdir -p "$OUT/$(dirname "$rel")"
  cp -n "$SRC/$rel" "$OUT/$rel" 2>/dev/null || true
}

while IFS= read -r ref; do
  case "$ref" in
    /_assets/*|/media/*|/atomicsite-fonts/*|/_*.js) copy_ref "${ref%%\#*}" ;;
  esac
done <<< "$refs"

# CSS can reference fonts and images the HTML never names directly.
# Capture the matches rather than piping grep straight into the loop: a
# stylesheet with no url() makes grep exit 1, which under `set -e` (and
# pipefail's equivalent trap) aborts the whole export mid-copy.
if [ -d "$OUT/_assets" ]; then
  for css in "$OUT"/_assets/*.css; do
    [ -f "$css" ] || continue
    css_urls="$(grep -ohE 'url\(/[^)]*\)' "$css" 2>/dev/null || true)"
    [ -n "$css_urls" ] || continue
    while IFS= read -r u; do
      u="$(printf '%s' "$u" | sed -E 's|url\(||; s|\)||; s|["'"'"']||g')"
      [ -n "$u" ] && copy_ref "${u%%\?*}"
    done <<< "$css_urls"
  done
fi

# ---- 4. repoint absolute URLs ----------------------------------------------
# Replace the bare hostname, not just "https://<host>". The CookieProof runtime
# config embeds it unprefixed ("domain":"<host>") to scope the consent cookie,
# and a bundle left pointing at the old host writes consent under a domain the
# visitor is not on.
find "$OUT" -type f \( -name '*.html' -o -name '*.txt' -o -name '*.xml' -o -name '*.js' \) \
  -exec sed -i '' "s|$SRC_HOST|$DOMAIN|g" {} +

# The CookieProof consent-proof endpoint is served by the Atomicsite app, not by
# the static bundle, so it cannot follow the site to a static host. Blank it
# rather than let the rewrite above point it at TARGET_DOMAIN, where it would
# 404 on every consent action. The banner itself is entirely client-side and
# keeps working; only server-side proof archival is off. Leaving it aimed at the
# ORIGIN host would work, but would send a client site's visitor consent records
# to our infrastructure, which is not ours to decide here.
for ccb in "$OUT"/_ccb.*.js; do
  [ -f "$ccb" ] || continue
  sed -i '' 's|"proofEndpoint":"[^"]*"|"proofEndpoint":""|g' "$ccb"
done

# ---- 5. regenerate robots + sitemap for the surviving pages -----------------
cat > "$OUT/robots.txt" <<EOF
User-agent: *
Allow: /

Sitemap: https://$DOMAIN/sitemap.xml
EOF

{
  echo '<?xml version="1.0" encoding="UTF-8"?>'
  echo '<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">'
  echo "  <url><loc>https://$DOMAIN/</loc></url>"
  for slug in "${SLUGS[@]}"; do
    echo "  <url><loc>https://$DOMAIN/$slug/</loc></url>"
  done
  echo '</urlset>'
} > "$OUT/sitemap.xml"

# ---- 6. verify --------------------------------------------------------------
fail=0
OUT_PAGES=("$OUT/index.html")
for slug in "${SLUGS[@]}"; do OUT_PAGES+=("$OUT/$slug/index.html"); done

leftover="$(grep -rl "$SRC_HOST" "$OUT" 2>/dev/null || true)"
if [ -n "$leftover" ]; then
  echo "FAIL: source host still present in:" >&2; echo "$leftover" >&2; fail=1
fi

missing=""
while IFS= read -r ref; do
  path="${ref%%\#*}"; path="${path%%\?*}"
  case "$path" in
    ""|/|"#"*|http*|mailto:*|tel:*|data:*) continue ;;
    /*) ;;
    *) continue ;;
  esac
  rel="${path#/}"
  if [ -f "$OUT/$rel" ] || [ -f "$OUT/$rel/index.html" ] || [ -f "$OUT/${rel%/}/index.html" ]; then
    continue
  fi
  missing="$missing$path\n"
done <<< "$(grep -ohE '(href|src)="/[^"]*"' "${OUT_PAGES[@]}" | sed -E 's/.*="//; s/"$//' | sort -u)"

if [ -n "$missing" ]; then
  echo "FAIL: dangling internal references:" >&2
  printf "%b" "$missing" | sed 's/^/  /' >&2
  fail=1
fi

echo
echo "pages    : $(find "$OUT" -name '*.html' | wc -l | tr -d ' ')"
echo "files    : $(find "$OUT" -type f | wc -l | tr -d ' ')"
echo "size     : $(du -sh "$OUT" | cut -f1)"
[ "$fail" -eq 0 ] && echo "verify   : OK" || echo "verify   : FAILED"
exit "$fail"
