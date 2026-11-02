// Calibrates the Studio's words for a decision (DESIGN §22.17): runs labelled
// questions on a real decision model and prints how the probabilities were
// distributed for right and wrong answers. Opt-in, operator-run, not in CI:
// it loads a real model. Run it on a scratch gateway only.
//
//   LOCALBASE_CALIBRATE_URL=http://127.0.0.1:<port> \
//   LOCALBASE_CALIBRATE_MODEL=clef-flash-q4_k_m \
//     bun run go/tools/decision-calibrate.ts [out.json]
//
// LOCALBASE_CALIBRATE_KEY is optional (a --no-auth gateway needs none). Stops
// if the gateway reports under 8 GiB of memory available.
const BASE = process.env.LOCALBASE_CALIBRATE_URL?.replace(/\/+$/, "");
const MODEL = process.env.LOCALBASE_CALIBRATE_MODEL;
if (!BASE || !MODEL) {
  console.error("decision-calibrate: set LOCALBASE_CALIBRATE_URL and LOCALBASE_CALIBRATE_MODEL");
  process.exit(2);
}
const KEY = process.env.LOCALBASE_CALIBRATE_KEY?.trim();
const out = process.argv[2];

type Case = { type: "choice" | "score" | "noul"; state: unknown; q: string; criteria?: unknown; expect: string | boolean | null; note?: string };

const invoice = { overdue: "Past the due date and unpaid", paid: "Already paid", open: "Not yet due and unpaid" };
const team = { billing: "Charges, refunds, invoices", shipping: "Delivery and tracking", technical: "Bugs and crashes" };
const sentiment = { positive: null, negative: null, neutral: null };
const urgency = ["low", "medium", "high"];
const stars = ["very bad", "bad", "okay", "good", "very good"];

// Labelled: the sets from decision-qualify.ts, then more of the same kinds.
const labelled: Case[] = [
  { type: "choice", state: { invoice: "INV-104", due: "2026-09-01", today: "2026-10-01", paid: false }, q: "What is the invoice status?", criteria: invoice, expect: "overdue" },
  { type: "choice", state: { invoice: "INV-9", due: "2026-12-01", today: "2026-10-01", paid: false }, q: "What is the invoice status?", criteria: invoice, expect: "open" },
  { type: "choice", state: { invoice: "INV-7", due: "2026-09-01", today: "2026-10-01", paid: true }, q: "What is the invoice status?", criteria: invoice, expect: "paid" },
  { type: "choice", state: "Customer message: I was charged twice for the same order.", q: "Which team should handle this ticket?", criteria: team, expect: "billing" },
  { type: "choice", state: "Customer message: My package never arrived and tracking has been stuck for two weeks.", q: "Which team should handle this ticket?", criteria: team, expect: "shipping" },
  { type: "choice", state: "Customer message: The app crashes every time I open the settings page.", q: "Which team should handle this ticket?", criteria: team, expect: "technical" },
  { type: "choice", state: "Customer message: Please refund my last invoice, I cancelled in March.", q: "Which team should handle this ticket?", criteria: team, expect: "billing" },
  { type: "choice", state: "Customer message: The courier left my parcel at the wrong address.", q: "Which team should handle this ticket?", criteria: team, expect: "shipping" },
  { type: "choice", state: "Customer message: I get a 500 error whenever I try to log in.", q: "Which team should handle this ticket?", criteria: team, expect: "technical" },
  { type: "choice", state: "Review: I love this, it works perfectly and setup took two minutes!", q: "What is the sentiment?", criteria: sentiment, expect: "positive" },
  { type: "choice", state: "Review: Terrible. It broke after one day and support ignored me.", q: "What is the sentiment?", criteria: sentiment, expect: "negative" },
  { type: "choice", state: "Review: The box contains one cable and a printed manual.", q: "What is the sentiment?", criteria: sentiment, expect: "neutral" },
  { type: "choice", state: "Review: Best purchase of the year, my whole family uses it daily.", q: "What is the sentiment?", criteria: sentiment, expect: "positive" },
  { type: "choice", state: "Review: Waste of money. Stopped charging within a week.", q: "What is the sentiment?", criteria: sentiment, expect: "negative" },
  { type: "choice", state: "Email: Your verification code is 482911.", q: "What kind of email is this?", criteria: { security: "Sign-in or verification", marketing: "Promotions and offers", personal: "From someone the reader knows" }, expect: "security" },
  { type: "choice", state: "Email: 40% off all shoes this weekend only, shop now!", q: "What kind of email is this?", criteria: { security: "Sign-in or verification", marketing: "Promotions and offers", personal: "From someone the reader knows" }, expect: "marketing" },
  { type: "choice", state: "Email: Hi Sam, are we still on for dinner with mum on Friday?", q: "What kind of email is this?", criteria: { security: "Sign-in or verification", marketing: "Promotions and offers", personal: "From someone the reader knows" }, expect: "personal" },
  { type: "choice", state: "Document: This agreement is made between the Landlord and the Tenant for the premises at 12 Oak Road.", q: "What type of document is this?", criteria: { contract: null, recipe: null, news: null, code: null }, expect: "contract" },
  { type: "choice", state: "Document: Whisk two eggs, fold in the flour and bake for twenty minutes.", q: "What type of document is this?", criteria: { contract: null, recipe: null, news: null, code: null }, expect: "recipe" },
  { type: "choice", state: "Document: The city council voted on Tuesday to extend the tram line to the airport.", q: "What type of document is this?", criteria: { contract: null, recipe: null, news: null, code: null }, expect: "news" },
  { type: "choice", state: "Document: for (const item of list) { total += item.price; }", q: "What type of document is this?", criteria: { contract: null, recipe: null, news: null, code: null }, expect: "code" },
  { type: "choice", state: { temperature_c: 39.2, cough: true, rash: false }, q: "Which is out of the normal range?", criteria: { temperature: null, none: "Everything is normal" }, expect: "temperature" },
  { type: "choice", state: "The meeting moved from Monday to Thursday because Ana is away on Monday.", q: "What day is the meeting now?", criteria: { monday: null, tuesday: null, wednesday: null, thursday: null }, expect: "thursday" },
  { type: "choice", state: "Log line: disk usage at 97% on /var, writes starting to fail.", q: "What is the problem area?", criteria: { storage: null, network: null, memory: null, auth: null }, expect: "storage" },
  { type: "choice", state: "Log line: connection timed out reaching db-2 on port 5432 after 3 retries.", q: "What is the problem area?", criteria: { storage: null, network: null, memory: null, auth: null }, expect: "network" },
  { type: "choice", state: "Log line: invalid password for user admin, account locked after 5 attempts.", q: "What is the problem area?", criteria: { storage: null, network: null, memory: null, auth: null }, expect: "auth" },
  { type: "noul", state: "Customer threatens legal action and demands a full refund today.", q: "Should a human follow up?", expect: true },
  { type: "noul", state: "Customer says thanks, the issue is resolved and they need nothing else.", q: "Should a human follow up?", expect: false },
  { type: "noul", state: { order: "A-1", status: "delivered" }, q: "Has the order shipped?", expect: true },
  { type: "noul", state: { order: "A-2", status: "cancelled before fulfilment" }, q: "Has the order shipped?", expect: false },
  { type: "noul", state: "The restaurant opens at 9 and closes at 17. It is now 12:30.", q: "Is the restaurant open?", expect: true },
  { type: "noul", state: "The restaurant opens at 9 and closes at 17. It is now 20:15.", q: "Is the restaurant open?", expect: false },
  { type: "noul", state: { age: 15, minimum_age: 18 }, q: "Is this person old enough?", expect: false },
  { type: "noul", state: { age: 34, minimum_age: 18 }, q: "Is this person old enough?", expect: true },
  { type: "noul", state: "Message: Congratulations! You won a free cruise, click this link and enter your card number.", q: "Is this message a scam?", expect: true },
  { type: "noul", state: "Message: Your dentist appointment is confirmed for Tuesday at 10.", q: "Is this message a scam?", expect: false },
  { type: "noul", state: "2 + 2 = 4", q: "Is this equation correct?", expect: true },
  { type: "noul", state: "7 x 6 = 41", q: "Is this equation correct?", expect: false },
  { type: "noul", state: "The ingredients are flour, butter, walnuts and sugar.", q: "Does this contain nuts?", expect: true },
  { type: "noul", state: "The ingredients are rice, tomatoes, onion and olive oil.", q: "Does this contain nuts?", expect: false },
  { type: "score", state: "Production database is down and every customer is affected.", q: "How urgent is this issue?", criteria: urgency, expect: "high" },
  { type: "score", state: "There is a small typo in the website footer.", q: "How urgent is this issue?", criteria: urgency, expect: "low" },
  { type: "score", state: "Checkout is slow for some users in one region, but orders still complete.", q: "How urgent is this issue?", criteria: urgency, expect: "medium" },
  { type: "score", state: "Payment processing has failed for all customers for the last hour.", q: "How urgent is this issue?", criteria: urgency, expect: "high" },
  { type: "score", state: "A colleague suggests changing the colour of an internal admin button.", q: "How urgent is this issue?", criteria: urgency, expect: "low" },
  { type: "score", state: "Review: Absolutely wonderful, exceeded every expectation.", q: "Rate this review.", criteria: stars, expect: "very good" },
  { type: "score", state: "Review: Awful. It arrived broken and smelled of smoke.", q: "Rate this review.", criteria: stars, expect: "very bad" },
  { type: "score", state: "Review: It does the job, nothing special either way.", q: "Rate this review.", criteria: stars, expect: "okay" },
];

// Labelled and tricky: negation, arithmetic, several steps. These are where
// wrong answers show up, so the bands can be tested against them.
const hard: Case[] = [
  { type: "noul", state: "The invoice was not unpaid.", q: "Is the invoice paid?", expect: true },
  { type: "noul", state: "I would never say the service was anything but excellent.", q: "Is the customer unhappy?", expect: false },
  { type: "noul", state: "Nobody who attended failed to sign the form. Maria attended.", q: "Did Maria sign the form?", expect: true },
  { type: "noul", state: "17 x 23 = 391", q: "Is this equation correct?", expect: true },
  { type: "noul", state: "144 / 12 = 13", q: "Is this equation correct?", expect: false },
  { type: "noul", state: "1234 + 8766 = 10000", q: "Is this equation correct?", expect: true },
  { type: "noul", state: "2027 is a leap year.", q: "Is this statement true?", expect: false },
  { type: "noul", state: "The train leaves at 14:50 and the trip takes 2 hours 25 minutes. The meeting starts at 17:00.", q: "Will the traveller arrive before the meeting?", expect: false },
  { type: "noul", state: "The train leaves at 14:20 and the trip takes 2 hours 25 minutes. The meeting starts at 17:00.", q: "Will the traveller arrive before the meeting?", expect: true },
  { type: "noul", state: { balance: 120, withdrawal: 75, fee: 50 }, q: "Is there enough money for the withdrawal and its fee?", expect: false },
  { type: "noul", state: { due: "2026-02-28", today: "2026-03-01", grace_days: 3 }, q: "Is the payment still within the grace period?", expect: true },
  { type: "noul", state: "All the managers are on the guest list. Priya is on the guest list.", q: "Does it follow that Priya is a manager?", expect: false },
  { type: "choice", state: "Ana is taller than Ben. Ben is taller than Chris.", q: "Who is the shortest?", criteria: { ana: null, ben: null, chris: null }, expect: "chris" },
  { type: "choice", state: "Ana is older than Ben, and Chris is older than Ana.", q: "Who is in the middle by age?", criteria: { ana: null, ben: null, chris: null }, expect: "ana" },
  { type: "choice", state: "Box A holds 12 items, box B holds twice as many as A minus 5, box C holds 18.", q: "Which box holds the most?", criteria: { a: null, b: null, c: null }, expect: "b" },
  { type: "choice", state: "The Monday meeting moved to Wednesday, and then the Wednesday meeting moved two days later.", q: "What day is the meeting now?", criteria: { monday: null, wednesday: null, thursday: null, friday: null }, expect: "friday" },
  { type: "choice", state: "Review: Oh great, another update that deletes my files. Just what I always wanted.", q: "What is the sentiment?", criteria: sentiment, expect: "negative" },
  { type: "choice", state: "Review: I expected to hate it, but honestly I can't stop using it.", q: "What is the sentiment?", criteria: sentiment, expect: "positive" },
  { type: "choice", state: "Review: Not bad at all, not bad at all.", q: "What is the sentiment?", criteria: sentiment, expect: "positive" },
  { type: "choice", state: "Customer message: Don't send the refund to billing, the parcel just never turned up.", q: "Which team should handle this ticket?", criteria: team, expect: "shipping" },
  { type: "choice", state: "Customer message: The tracking page itself throws an error when I open it; the parcel arrived fine.", q: "Which team should handle this ticket?", criteria: team, expect: "technical" },
  { type: "choice", state: "A 12 pack costs 18 dollars. A 20 pack costs 28 dollars. A 6 pack costs 10 dollars.", q: "Which is cheapest per item?", criteria: { six: null, twelve: null, twenty: null }, expect: "twenty" },
  { type: "choice", state: "It is 23:30 on Tuesday in a city. The flight lands 90 minutes later, same time zone.", q: "What day does it land?", criteria: { tuesday: null, wednesday: null, thursday: null }, expect: "wednesday" },
  { type: "choice", state: "The word is 'bookkeeper'.", q: "Which letter appears most often?", criteria: { b: null, o: null, k: null, e: null }, expect: "e" },
  { type: "score", state: "A security researcher reports customer passwords are readable in a public log file.", q: "How urgent is this issue?", criteria: urgency, expect: "high" },
  { type: "score", state: "The office printer in a closed branch is out of toner.", q: "How urgent is this issue?", criteria: urgency, expect: "low" },
  { type: "score", state: "Review: I wanted to love it. I really did. It's going back tomorrow.", q: "Rate this review.", criteria: stars, expect: "bad" },
  { type: "score", state: "Review: Not terrible, I suppose, though I have used better.", q: "Rate this review.", criteria: stars, expect: "okay" },
];

// Hard on purpose: no right answer, or one a person would hesitate over. These
// show what a close call looks like; they do not count towards accuracy.
const ambiguous: Case[] = [
  { type: "choice", state: "Customer message: The app charged me twice after it crashed during checkout.", q: "Which team should handle this ticket?", criteria: team, expect: null },
  { type: "choice", state: "Customer message: My invoice says delivered but the parcel never came, and I want my money back.", q: "Which team should handle this ticket?", criteria: team, expect: null },
  { type: "choice", state: "Customer message: Hello.", q: "Which team should handle this ticket?", criteria: team, expect: null },
  { type: "choice", state: "Review: Great battery, but the screen scratched in a day and the price is steep.", q: "What is the sentiment?", criteria: sentiment, expect: null },
  { type: "choice", state: "Review: Well, it certainly is a product.", q: "What is the sentiment?", criteria: sentiment, expect: null },
  { type: "choice", state: "A company has a small budget and wants to reach new customers.", q: "What should they try first?", criteria: { email: null, social: null, referrals: null, events: null }, expect: null },
  { type: "choice", state: "Two candidates: one has ten years of experience, the other has the stronger references.", q: "Who should be hired?", criteria: { experienced: null, referenced: null }, expect: null },
  { type: "choice", state: "The weather forecast says 50% chance of rain this afternoon.", q: "What should I bring?", criteria: { umbrella: null, sunglasses: null }, expect: null },
  { type: "choice", state: "xqzv plm", q: "What language is this?", criteria: { english: null, french: null, german: null, spanish: null }, expect: null },
  { type: "noul", state: "Customer: I guess it's fine, though I might call again later.", q: "Should a human follow up?", expect: null },
  { type: "noul", state: "It might rain tomorrow, or it might not.", q: "Will it rain tomorrow?", expect: null },
  { type: "noul", state: "A coin is about to be flipped.", q: "Will it land heads?", expect: null },
  { type: "noul", state: "The email looks like it is from the bank, with the right logo, but asks me to confirm details on a new site.", q: "Is this message a scam?", expect: null },
  { type: "noul", state: "The contract renews automatically unless cancelled, and the notice period is unclear.", q: "Is it too late to cancel?", expect: null },
  { type: "score", state: "Some users say the app feels a bit slower since last week; no errors reported.", q: "How urgent is this issue?", criteria: urgency, expect: null },
  { type: "score", state: "Review: Some good, some bad, depends on the day.", q: "Rate this review.", criteria: stars, expect: null },
  { type: "score", state: "A customer in one region cannot pay with one card type; other cards work.", q: "How urgent is this issue?", criteria: urgency, expect: null },
];

async function available(): Promise<number | null> {
  try {
    const html = await (await fetch(`${BASE}/app/models/running`)).text();
    const m = html.match(/([0-9.]+) GiB free of/);
    return m ? Number(m[1]) : null;
  } catch {
    return null;
  }
}

const rows: any[] = [];
for (const [set, cases] of [["labelled", labelled], ["hard", hard], ["ambiguous", ambiguous]] as const) {
  for (const c of cases) {
    const free = await available();
    if (free !== null && free < 8) {
      console.error(`stopping: only ${free} GiB available`);
      process.exit(1);
    }
    const question: any = { type: c.type, instructions: c.q };
    if (c.criteria) question.criteria = c.criteria;
    const res = await fetch(`${BASE}/v1/systemone`, {
      method: "POST",
      headers: { "content-type": "application/json", ...(KEY ? { authorization: `Bearer ${KEY}` } : {}) },
      body: JSON.stringify({ model: MODEL, state: c.state, questions: { q: question } }),
      signal: AbortSignal.timeout(120_000),
    });
    const json: any = await res.json();
    const a = json?.answers?.q;
    if (!res.ok || !a) {
      console.error(`failed: ${c.q}: ${res.status} ${JSON.stringify(json).slice(0, 160)}`);
      continue;
    }
    // One shape for all three types: the options and their probabilities.
    let probs: Record<string, number>;
    if (c.type === "noul") probs = { yes: a.noul, no: 1 - a.noul };
    else if (c.type === "score") probs = Object.fromEntries(Object.entries(a.probabilities).map(([i, p]) => [a.legend[i], p as number]));
    else probs = a.probabilities;
    const sorted = Object.entries(probs).sort((x, y) => y[1] - x[1]);
    const top = sorted[0][1], gap = top - (sorted[1]?.[1] ?? 0);
    const want = c.type === "noul" && c.expect !== null ? (c.expect ? "yes" : "no") : c.expect;
    const row = { set, type: c.type, q: c.q, state: typeof c.state === "string" ? c.state.slice(0, 60) : JSON.stringify(c.state).slice(0, 60),
      answer: sorted[0][0], top: +top.toFixed(4), gap: +gap.toFixed(4), confidence: a.confidence ?? null, options: sorted.length, expect: want, right: want === null ? null : sorted[0][0] === want };
    rows.push(row);
    console.log(`${row.right === null ? "?" : row.right ? "ok" : "XX"}\t${row.type}\ttop ${row.top.toFixed(3)}\tgap ${row.gap.toFixed(3)}\tconf ${row.confidence?.toFixed?.(3) ?? "-"}\t${row.answer}\t${row.q.slice(0, 34)} / ${row.state.slice(0, 40)}`);
  }
}
for (const set of ["labelled", "hard"]) {
  const lab = rows.filter((r) => r.set === set);
  console.log(`${set}: ${lab.filter((r) => r.right).length}/${lab.length} right`);
}
// Each band candidate: how many graded answers fall in it, and how many are right.
const graded = rows.filter((r) => r.right !== null);
for (const min of [0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 0.95]) {
  const hit = graded.filter((r) => r.gap >= min);
  console.log(`gap >= ${min}: ${hit.filter((r) => r.right).length}/${hit.length} right; ambiguous in it: ${rows.filter((r) => r.set === "ambiguous" && r.gap >= min).length}`);
}
if (out) await Bun.write(out, JSON.stringify(rows, null, 1));
