const apiKey = process.env.LINEAR_API_KEY;

if (!apiKey || apiKey.trim() === "" || apiKey.includes("your_api_key_here") || apiKey.startsWith("your_")) {
  console.error("Error: LINEAR_API_KEY is missing, blank, or contains a placeholder value in .env.");
  process.exit(1);
}

const proc = Bun.spawn({
  cmd: [
    "bunx",
    "mcp-remote",
    "https://mcp.linear.app/mcp",
    "--header",
    `Authorization: Bearer ${apiKey}`
  ],
  stdout: "inherit",
  stderr: "inherit",
  stdin: "inherit",
});

await proc.exited;
