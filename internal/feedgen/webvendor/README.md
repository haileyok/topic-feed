# The vendored browser OAuth client

The page at `/feeds` publishes a person's feeds from their own browser: it signs in to their Bluesky
account with OAuth (the tokens stay in the browser) and writes the feed records itself. For that it uses
Bluesky's own client library, `@atproto/oauth-client-browser`, bundled into one file:

    web/static/vendor/atproto-oauth-client-browser.js            (the script the page loads)
    web/static/vendor/atproto-oauth-client-browser.LICENSES.txt  (the licences of what is in it, with their texts)

The service has no build step for its web pages, so the bundle is built here by hand and committed.
Nothing else is taken from the library: `@atproto/api` (which is four times the size) is not used; the
page calls the account's server directly with the session the client gives it.

## Rebuilding (to change the version)

    cd internal/feedgen/webvendor
    npm install
    npm run build

Versions are pinned in `package.json` (library 0.5.9, esbuild 0.28.2). After changing them, run the page's
tests (`TOPICFEED_JSDOM=... go test ./internal/feedgen`), try signing in and publishing a feed in a
browser, and check that the bundle does not contain `eval(` or `new Function` (the page's content
security policy forbids both): `grep -c 'eval(\|new Function' ../web/static/vendor/*.js` should say 0.

Size at 0.5.9: about 213 kB, 59 kB gzipped. `node_modules` is not committed.
