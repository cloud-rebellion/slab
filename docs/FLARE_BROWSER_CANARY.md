# Atomicsite Flare browser source-map canary

Use this after an Atomicsite deploy to prove that a handled error from the
public `/login` page resolves through the private source map for the exact
deployed browser bundle. The canary is dormant until an operator dispatches
its event in that page. It sends one fixed marker per page through the normal
browser Flare sender and its existing rate limit. It accepts no event detail.

## Before opening the page

- Confirm the deployed image and full source commit. Its browser build must have
  `PUBLIC_FLARE_DSN` set and `PUBLIC_FLARE_RELEASE` equal to that source commit.
- Confirm the matching private JavaScript maps were uploaded to the same Flare
  project and release. The maps must not be served with the public site.
- Use a fresh tab and keep the browser Network panel open. A page where the
  sender already exhausted its per-minute budget may consume the once-per-page
  canary without sending it.

## Browser check

1. Open the deployed public `https://<atomicsite-host>/login` page and hard
   reload. In DevTools Sources, search for the exact string
   `Atomicsite Flare browser source-map canary v1`. Copy the URL of the loaded
   `/_app/immutable/nodes/<name>.<hash>.js` file containing it. The filename
   changes with each build; the code must be in a real JavaScript response,
   not a console `sourceURL` or inline snippet.
2. In DevTools Network, confirm that file loaded successfully. For the exact
   copied URL, `document.scripts` must contain it or
   `performance.getEntriesByName(url, 'resource')` must show an initiator type
   of `script` or `link`. This is the evidence the bridge requires before it
   assigns a source-map asset name to the stack frame.
3. In the Console run:

   ```js
   window.dispatchEvent(new Event('flare:browser-canary'));
   ```

   The page should stay usable. A second dispatch in the same page should not
   queue another canary. In Network, look for one request to the configured
   Flare ingest `/api/<project-id>/events` endpoint. A queued `sendBeacon`
   request alone is not proof that Flare stored the event. Do not copy the
   request URL or payload into shared notes.

## Flare UI check

1. Open the receiving Flare project, then **Issues**. Find the new
   `FlareBrowserCanary` issue with message
   `Atomicsite Flare browser source-map canary v1`; open its latest event.
   Confirm its receive time, `/login` request URL, and `release` equal to the
   exact deployed source commit. Grouping can put the newest event in an
   existing issue, so use the event time and release.
2. The stack trace should show **source-mapped** and a **mapped** frame in
   `src/lib/flare-client.ts` at the canary handler. This is the end-to-end
   symbolication result. In the project's **Source maps** list, check for a
   matching artifact at that release whose name is
   `flare-asset-sha256:<SHA-256 of script URL origin + pathname>`.
3. If the frame says `unknown`, recheck the exact browser script URL and the
   loaded-script evidence in step 2. If the frame has an asset hash but is not
   mapped, recheck the image release, the artifact release and name, and that
   the private map came from that same build. If no event appears, inspect the
   browser's ingest request and `connect-src` result, then the configured DSN,
   project, and sender budget. Do not broaden tenant CSP for this check.

The local build can verify that the marker is in a generated immutable chunk
and that its hidden map points to `flare-client.ts`. Only the deployed browser
request and the Flare event can prove delivery and live symbolication. This
canary covers one handled stack frame in one bundle; it does not prove other
chunks, unhandled-error capture, alert delivery, or authenticated journeys.
