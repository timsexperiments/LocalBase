import { join } from "node:path";
import { z } from "zod";
import type { ServeInput } from "../app/commands/inputs";
import { hostSchema, portSchema } from "../config/schema";

const gatewayListenerSchema = z
  .object({
    host: hostSchema.default("127.0.0.1"),
    port: portSchema.default(2273),
  })
  .strict();

export async function loadGatewayListener(
  root: string,
  overrides: Pick<ServeInput, "host" | "port"> = {},
) {
  let contents = "{}";
  try {
    contents = await Bun.file(join(root, "gateway-listener.json")).text();
  } catch (error) {
    if (!(error instanceof Error && "code" in error && error.code === "ENOENT"))
      throw error;
  }
  let listener: z.infer<typeof gatewayListenerSchema>;
  try {
    listener = gatewayListenerSchema.parse(JSON.parse(contents));
  } catch {
    throw new Error(
      "Invalid gateway-listener.json. Expected only host and integer port (1-65535).",
    );
  }
  return {
    host: overrides.host ?? listener.host,
    port: overrides.port ?? listener.port,
  };
}
