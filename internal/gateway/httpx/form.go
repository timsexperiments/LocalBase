package httpx

// MaxFormParts is how many parts a form may have. Every part costs memory however little is in it,
// so the body limit alone lets a form cost gigabytes. These routes take a few fields each, and this
// is the limit the standard library sets for its own form reader.
const MaxFormParts = 1000
