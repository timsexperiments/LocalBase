export type BrowserAccessErrorCode =
  | "policy-not-configured"
  | "managed-user-not-found"
  | "managed-user-exists"
  | "role-not-found"
  | "policy-conflict"
  | "policy-revision-conflict";

export class BrowserAccessError extends Error {
  constructor(
    readonly code: BrowserAccessErrorCode,
    message: string,
  ) {
    super(message);
    this.name = "BrowserAccessError";
  }
}
