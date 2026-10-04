// The header every page shares: the same links on every page, and who is signed in, with the way to
// sign in or out. The pages write the header's markup themselves (so the links work before this runs);
// this fills in the account part, and shows the post inspector's link to the owner, the only account
// that may use it.
//
// Everything goes on the page through textContent, never as HTML: a handle is whatever its owner chose.

const $ = (id) => document.getElementById(id);

// whoAmI is who /api/me says is signed in: {did, handle, owner} when someone is, false when nobody
// is, and null when it can't be told (sign-in is switched off, or the request failed).
export async function whoAmI() {
  let res;
  try {
    res = await fetch("/api/me", { headers: { Accept: "application/json" }, credentials: "same-origin" });
  } catch {
    return null;
  }
  if (res.status === 401) return false;
  if (!res.ok) return null;
  try {
    const me = await res.json();
    if (!me || typeof me.did !== "string" || !me.did) return null;
    return { did: me.did, handle: typeof me.handle === "string" ? me.handle : "", owner: me.owner === true };
  } catch {
    return null;
  }
}

async function signOut() {
  const button = $("account-signout");
  const problem = $("account-problem");
  button.disabled = true;
  problem.hidden = true;
  let ok = false;
  try {
    const res = await fetch("/oauth/logout", { method: "POST", headers: { Accept: "application/json" }, credentials: "same-origin" });
    ok = res.ok;
  } catch {
    // reported below
  }
  if (ok) {
    location.reload(); // signed out: every page starts again as it is for someone signed out
    return;
  }
  // Still signed in, and the person should know.
  problem.textContent = "Couldn't sign out. Try again.";
  problem.hidden = false;
  button.disabled = false;
}

// setupHeader fills in the account part of the header. Its state (data-state of #account) is
// "checking" until /api/me answers, then "signed-in", "signed-out", or "unknown" (nothing is shown:
// sign-in is off here, or the server couldn't be asked).
export async function setupHeader() {
  const box = $("account");
  if (!box) return;
  const me = await whoAmI();
  if (me === null) {
    box.dataset.state = "unknown";
    return;
  }
  if (me === false) {
    $("account-signin").hidden = false;
    box.dataset.state = "signed-out";
    return;
  }
  const who = $("account-who");
  who.textContent = me.handle ? "@" + me.handle : me.did;
  who.title = me.did;
  who.hidden = false;
  const out = $("account-signout");
  out.hidden = false;
  out.addEventListener("click", signOut);
  const inspect = $("nav-inspect");
  if (inspect) inspect.hidden = !me.owner;
  box.dataset.state = "signed-in";
}

// On a narrow screen the links scroll sideways: start with this page's link in view.
function showCurrentLink() {
  const nav = document.querySelector(".sitenav");
  const link = nav && nav.querySelector('[aria-current="page"]');
  if (link && nav.scrollWidth > nav.clientWidth) nav.scrollLeft = link.offsetLeft - nav.offsetLeft - (nav.clientWidth - link.offsetWidth) / 2;
}

showCurrentLink();
setupHeader().then(showCurrentLink); // the account part takes room from the links once it's filled in
