import type { AppContext } from "../../../context";
import type { CommandExecution } from "../../app/commands/framework";
import type { DoctorInput } from "../../app/commands/inputs";
import { publicConfiguration } from "../../app/commands/results";

export function runDoctor(
  _input: DoctorInput,
  ctx: AppContext,
  execution: CommandExecution,
): {
  data: {
    hardware: AppContext["specs"];
    configuration: ReturnType<typeof publicConfiguration>;
  };
} {
  const specs = ctx.specs;

  execution.output.info(`OS: ${specs.osName}`);
  execution.output.info(`CPU: ${specs.cpuModel}`);
  execution.output.info(`RAM: ${specs.ramGb} GB`);
  execution.output.info(`GPU: ${specs.gpuName}`);
  if (specs.isAppleSilicon) {
    execution.output.info(`GPU VRAM: ${specs.gpuVramGb} GB (Unified Memory)`);
  } else {
    execution.output.info(`GPU VRAM: ${specs.gpuVramGb} GB (Discrete Memory)`);
  }
  execution.output.info(`Parallel Slots: ${ctx.config.parallel}`);

  if (specs.isAppleSilicon) {
    if (specs.ramGb >= 64) {
      execution.output.info(
        "Status: ✅ Excellent fit for large developer-grade models (up to 32B/70B) using unified memory.",
      );
    } else if (specs.ramGb >= 32) {
      execution.output.info(
        "Status: ✅ Good fit for medium coding models (up to 14B/32B) using unified memory.",
      );
    } else if (specs.ramGb >= 16) {
      execution.output.info(
        "Status: ⚠️ Good for entry coding models (1.5B/7B), but larger models may slow down.",
      );
    } else {
      execution.output.info(
        "Status: ❌ Low memory. Limit to small models (1.5B) and small contexts.",
      );
    }
  } else {
    if (specs.gpuVramGb >= 16) {
      execution.output.info(
        "Status: ✅ Excellent fit for multi-model coding setup (7B/14B models + local STT).",
      );
    } else if (specs.gpuVramGb >= 12) {
      execution.output.info(
        "Status: ✅ Good fit for 7B models with plenty of context headroom.",
      );
    } else if (specs.gpuVramGb >= 8) {
      execution.output.info(
        "Status: ⚠️ Tight fit for 7B models (warning: context growth may slow down), or perfect for 3B/1.5B models.",
      );
    } else {
      execution.output.info(
        "Status: ❌ Low VRAM. Focus on 1.5B/3B models or CPU offloading.",
      );
    }
  }
  return {
    data: { hardware: specs, configuration: publicConfiguration(ctx.config) },
  };
}
