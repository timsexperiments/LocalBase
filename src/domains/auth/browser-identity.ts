export type BrowserIdentity = Readonly<{
  issuer: string;
  subject: string;
  verifiedEmail?: string;
}>;
