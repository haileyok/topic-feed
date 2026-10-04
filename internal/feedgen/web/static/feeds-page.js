// The page at /feeds starts here: it loads the OAuth client library (a bundle of Bluesky's own, in vendor/),
// and hands it to the page's script. Without it (it failed to load) the page still lists feeds and
// can delete and edit them; it just can't publish.

import "./header.js"; // the header every page shares
import { main } from "./feeds.js";

let ClientClass = null;
try {
  ({ BrowserOAuthClient: ClientClass } = await import("./vendor/atproto-oauth-client-browser.js"));
} catch {
  // the page says it can't publish
}
main({ ClientClass });
