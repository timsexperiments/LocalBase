// Whose words these are. What Studio keeps in this browser is under a key for the person, so the
// next person on this browser doesn't get it, and a tab doesn't hold on to it for them.

export const personKey = () => document.documentElement.dataset.person ?? "none";

// What a tab holds of a person's words: a prompt on its way to another tool, and a question.
const TAB = ["localbase.lab.fill", "localbase.decisions."];
const held = () => Object.keys(sessionStorage).filter((key) => TAB.some((prefix) => key.startsWith(prefix)));

function forget(keep) {
  try {
    for (const key of held()) if (!keep(key)) sessionStorage.removeItem(key);
  } catch {
    // Storage blocked: then nothing was kept either.
  }
}

// At sign-out.
export const forgetTab = () => forget(() => false);

// On every page: someone else's, or from before keys were per person, goes before anything can read it.
// A session that ended on its own lands here too, so this is what covers a redirect to sign-in.
export const forgetOthers = () => forget((key) => key.endsWith(`.v2.${personKey()}`));
