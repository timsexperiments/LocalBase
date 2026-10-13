import { afterAll, beforeAll, spyOn } from "bun:test";
import * as manager from "../manager";

export function useSupportedVideoHost() {
  let restore = () => {};
  beforeAll(() => {
    const target = spyOn(manager, "detectHostVideoTarget").mockReturnValue({
      platform: "linux",
      architecture: "x64",
      accelerator: "nvidia",
    });
    restore = () => target.mockRestore();
  });
  afterAll(() => restore());
}
