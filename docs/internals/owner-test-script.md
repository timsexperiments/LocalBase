# Owner's test script

**Nothing from the UI v2 stack goes live until step 1 passes on a real phone.**

What only a real device can prove. Automated checks run in headless Chrome on a scratch
gateway with a fake sign-in provider and fake models, so none of this has been done by them.

## 1. Sign in with Google on your phone

Do this on a test gateway running the new build, at its public address.

1. On your phone, in a private tab, open the public address. Google's sign-in should appear.
2. Sign in. You should land in LocalBase, signed in as your own address.
3. Open **More**, then **Sign out**. You should see "You're signed out", and stay signed out.
4. Reload that page. You should still be signed out.
5. Tap **Sign in again**. You should get back in.
6. Close the tab, open the address again. You should still be signed in.

**Switching accounts.** This has only been tested against a fake provider. If you have a second
Google account (if not, skip this and tell the operator it was skipped):

7. Sign out, then tap **Use another account**. Google should ask which account to use.
8. Choose the second account. If your policy doesn't name it, you should see "You're signed in, but
   this account doesn't have access yet", with that account's address.
9. Tap **Use another account** there, and choose your own. You should get back in.
10. Sign out, then tap **Sign in again**. It should not ask which account, or at most one tap.

**If it breaks:** nothing is lost. Keep the current build live. Once the first-run link is live,
on the Mac run `local-base access login-link` and open the link it prints in a browser on the
Mac: that signs in locally without Google. Tell the operator which step failed and what it
showed.

## 2. Someone who isn't on your list

Sign in with a Google account your access policy doesn't name. You should see "You're signed in,
but this account doesn't have access yet", with that account's address and Sign out, on every
address you try. Sign out should work.

## 3. Safari on the Mac

The first-run link is only tested in Chrome, and Safari's handling of its cookie is why the
cookie isn't `Secure`.

1. On a fresh install, run `local-base access login-link`.
2. Open the link it prints in Safari. You should land signed in.
3. Reload. You should stay signed in.
4. **Sign out** should show "You're signed out".

## 4. The microphone, on your phone

Dictation and Transcribe have only been tested with a generated tone.

1. Allow the microphone when asked, and dictate a sentence.
2. Deny it in a fresh tab. It should say so plainly.
3. Switch away from the browser in the middle of a recording, then come back.
4. Record for three minutes.
5. Record very quietly, then very loudly.
6. Turn off the connection before sending a recording. It should fail plainly and keep your place.

## 5. Speech, by ear

Speech has been checked by analysis, not by listening.

- Numbers and long strings of digits can be cut or repeated.
- Listen for clicks at the joins between parts.

## 6. The keyboard, on your phone

The keyboard has only been simulated by shrinking the page.

1. In Chat, tap the message box. The composer should sit on top of the keyboard, with the strip
   and its thin gauge above.
2. Do the same in Studio's Image and Decisions.
