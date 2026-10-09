export function shouldShowOperationalOutput(): boolean {
  return !(
    process.env.LOCALBASE_QUIET_TEST_LOGS === "1" &&
    process.env.LOCALBASE_TEST_LOGS !== "1"
  );
}
