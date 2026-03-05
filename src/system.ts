import { existsSync, readFileSync } from "node:fs";
import { spawnSync } from "node:child_process";

export type HostSpecs = {
  osName: string;
  ramGb: number;
  cpuModel: string;
  gpuName: string;
  gpuVramGb: number;
};

function run(cmd: string, args: string[]): string {
  const result = spawnSync(cmd, args, { encoding: "utf8", timeout: 3000, killSignal: "SIGKILL" });
  return (result.stdout ?? "").trim();
}

export function detectSpecs(): HostSpecs {
  const osName = run("bash", ["-lc", "source /etc/os-release && echo $PRETTY_NAME"]) || "Unknown";

  let ramKb = 0;
  if (existsSync("/proc/meminfo")) {
    const memInfo = readFileSync("/proc/meminfo", "utf8").split("\n");
    const memLine = memInfo.find((line) => line.startsWith("MemTotal:"));
    if (memLine) {
      ramKb = Number(memLine.split(/\s+/)[1]);
    }
  }
  const ramGb = Math.round(ramKb / 1024 / 1024);

  let cpuModel = "Unknown";
  if (existsSync("/proc/cpuinfo")) {
    const cpuInfo = readFileSync("/proc/cpuinfo", "utf8").split("\n");
    const line = cpuInfo.find((l) => l.startsWith("model name"));
    if (line) {
      cpuModel = line.split(":", 2)[1]?.trim() ?? cpuModel;
    }
  }

  let gpuName = "Unavailable";
  let gpuVramGb = 0;
  const smi = spawnSync("bash", ["-lc", "command -v nvidia-smi"], { encoding: "utf8", timeout: 3000, killSignal: "SIGKILL" });
  if (smi.status === 0) {
    const raw = run("nvidia-smi", ["--query-gpu=name,memory.total", "--format=csv,noheader,nounits"]);
    const first = raw.split("\n")[0] ?? "";
    if (first.includes(",")) {
      const [name, memMbRaw] = first.split(",", 2).map((item) => item.trim());
      const memMb = Number(memMbRaw);
      if (name) {
        gpuName = name;
      }
      if (Number.isFinite(memMb) && memMb > 0) {
        gpuVramGb = Math.round(memMb / 1024);
      }
    }
  }

  return { osName, ramGb, cpuModel, gpuName, gpuVramGb };
}
