import type { AppContext } from "../../../context";

export interface CommandFlag {
  name: string;
  type: string;
  description: string;
  short?: string;
}

export interface CLICommand {
  name: string;
  description: string;
  positional?: string[];
  flags?: CommandFlag[];
  requiresDatabase?: boolean;
  handler: (args: string[], ctx: AppContext) => Promise<number> | number;
}
