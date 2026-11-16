// Whether a request is the page's own (a poll, a load or reveal trigger), not the person's. DESIGN §22.3a.
export const background = (elt) => !!elt?.getAttribute && (!!elt.hasAttribute?.("data-poll") || /\b(every|load|revealed|intersect)\b/.test(elt.getAttribute("hx-trigger") ?? ""));
