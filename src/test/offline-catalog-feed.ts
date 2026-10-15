// Tests never download the published catalog feed. Feed tests pass their
// own URL (a local server) or environment explicitly; spawned CLI and
// gateway processes inherit this.
process.env.LOCALBASE_CATALOG_FEED_URL ??= "off";
