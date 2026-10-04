// The sign-in form, the same on every page that needs someone signed in (/me, /feeds, /inspect): a
// Bluesky handle, then off to the person's own server to approve, and back to the page it started from.
//
// The sign-in endpoints answer JSON when asked, because the pages' content security policy doesn't let
// a form post away and a script can't read where a redirect leads. All text is put on the page with
// textContent, never as HTML.

const $ = (id) => document.getElementById(id);

// What the sign-in endpoints and the callback report, in words.
export const MESSAGES = {
  denied: "Sign-in was cancelled.",
  invalid: "That doesn't look like a Bluesky handle. Try something like alice.bsky.social.",
  busy: "Lots of people are signing in right now. Try again in a minute.",
  failed: "Sign-in didn't work. Check the handle and try again.",
  limited: "That's been tried a lot. Wait a few seconds and try again.",
  network: "Couldn't reach the server. Check your connection and try again.",
  off: "Signing in isn't turned on for this site yet.",
};

export function messageFor(code) {
  return Object.hasOwn(MESSAGES, code) ? MESSAGES[code] : MESSAGES.failed;
}

/** notice says something above the form, or nothing. */
export function notice(text) {
  const n = $("signin-notice");
  n.textContent = text || "";
  n.hidden = !text;
}

// startLogin asks where to send the browser to sign in the account, and returns that address. The
// page it is on goes along, so signing in comes back to it (the server only goes back to its own pages).
async function startLogin(handle) {
  let res;
  try {
    res = await fetch("/oauth/login", {
      method: "POST",
      headers: { Accept: "application/json", "Content-Type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({ handle, return: location.pathname }),
      credentials: "same-origin",
    });
  } catch {
    throw new Error("network");
  }
  let body = {};
  try {
    body = await res.json();
  } catch {
    // not JSON: handled below
  }
  if (res.ok && typeof body.redirect === "string") {
    const url = new URL(body.redirect); // throws on nonsense
    if (url.protocol !== "https:" && url.protocol !== "http:") throw new Error("failed");
    return url.href;
  }
  throw new Error(typeof body.error === "string" ? body.error : "failed");
}

/**
 * setupSignInForm makes the form work, and takes a problem the last sign-in came back with (?signin=...)
 * out of the address bar. It returns that problem's code, or null: the page shows it with
 * showSignInProblem once it knows nobody is signed in.
 */
export function setupSignInForm() {
  const problem = new URLSearchParams(location.search).get("signin");
  if (problem !== null) {
    const q = new URLSearchParams(location.search);
    q.delete("signin");
    const rest = q.toString();
    history.replaceState(null, "", location.pathname + (rest ? "?" + rest : "") + location.hash);
  }
  const form = $("login");
  const button = $("login-button");
  const label = button.textContent;
  const reset = () => {
    button.disabled = false;
    button.textContent = label;
  };
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    const handle = $("handle").value.trim();
    if (!handle) {
      notice(MESSAGES.invalid);
      return;
    }
    notice("");
    button.disabled = true;
    button.textContent = "Opening Bluesky…";
    try {
      location.assign(await startLogin(handle));
    } catch (err) {
      notice(messageFor(err.message));
      reset();
    }
  });
  // Coming back with the browser's back button restores this page as it was left.
  document.defaultView.addEventListener("pageshow", (ev) => {
    if (ev.persisted) reset();
  });
  return problem;
}

/** showSignInProblem says what went wrong with the last sign-in, if anything did. */
export function showSignInProblem(code) {
  if (code) notice(messageFor(code));
}

/** signInOff says signing in isn't possible here, and turns the form off. */
export function signInOff() {
  notice(MESSAGES.off);
  $("handle").disabled = true;
  $("login-button").disabled = true;
}
