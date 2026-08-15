import type { ILogger } from "../observability/logging";
import { internalSpanOptions, type OtelRuntime } from "../observability/otel";
import { guardianProcessCommand } from "./backend-guardian";
import type { ModalityLifecycleState } from "./health";
import {
  RuntimeMemoryAdmissionError,
  type MemorySafetyController,
  type RuntimeMemoryReservation,
} from "./memory-controller";
import type { RuntimeComponent, RuntimeModality } from "./modality";
import type { RuntimeLaunchPlan } from "./launch-plan";

const CHILD_STOP_GRACE_MS = 500;
const HEALTH_PROBE_TIMEOUT_MS = 2_000;

class StartupCancelledError extends Error {
  constructor(name: string) {
    super(`${name} startup was cancelled.`);
    this.name = "StartupCancelledError";
  }
}

class CrashLimitReachedError extends Error {
  constructor(name: string) {
    super(`${name} is unavailable after repeated failures.`);
    this.name = "CrashLimitReachedError";
  }
}

type StartupAttempt = {
  generation: number;
  controller: AbortController;
  cancelled: boolean;
};

export type ManagedServiceOptions = {
  runtimeId: string;
  modality: RuntimeModality;
  component: RuntimeComponent;
  healthUrl: string;
  logger: ILogger;
  launch: () => Promise<RuntimeLaunchPlan>;
  start: (plan: RuntimeLaunchPlan) => Promise<Bun.Subprocess>;
  memorySafety: MemorySafetyController;
  otel: OtelRuntime;
  startupTimeoutMs?: number;
  onFatal?: () => Promise<void>;
};

/** Manages one lazily-started backend process and its recovery lifecycle. */
export class ManagedService {
  private proc: Bun.Subprocess | null = null;
  private crashTimes: number[] = [];
  private isRestarting = false;
  private restartPromise: Promise<void> | null = null;
  private isShuttingDown = false;
  private shutdownPromise: Promise<void> | null = null;
  private guardians = new Map<number, Bun.Subprocess>();
  private expectedStops = new WeakSet<Bun.Subprocess>();
  private startupGeneration = 0;
  private startup: StartupAttempt | null = null;
  private lifecycleState: ModalityLifecycleState = "idle";
  private reservation: RuntimeMemoryReservation | null = null;

  constructor(private readonly options: ManagedServiceOptions) {}

  private get name(): RuntimeComponent {
    return this.options.component;
  }

  private lifecycle(
    eventName:
      | "backend.starting"
      | "backend.ready"
      | "backend.restart-backoff"
      | "backend.crash"
      | "backend.stopping"
      | "backend.stopped",
    severity: "info" | "warn" | "error",
    attributes?: Record<string, unknown>,
  ): void {
    this.options.logger.event({
      severity,
      eventName,
      category: "runtime",
      component: this.name,
      runtime: this.options.modality,
      message: eventName,
      attributes,
    });
  }

  state(): ModalityLifecycleState {
    return this.lifecycleState;
  }

  runtimeId(): string {
    return this.options.runtimeId;
  }

  private exited(proc: Bun.Subprocess): boolean {
    return proc.exitCode !== null || proc.signalCode != null;
  }

  private unavailableError(): Error {
    return new Error(`${this.name} is unavailable after repeated failures.`);
  }

  private failed(): boolean {
    return this.lifecycleState === "failed";
  }

  private startupIsActive(attempt: StartupAttempt): boolean {
    return (
      this.startup?.generation === attempt.generation && !attempt.cancelled
    );
  }

  private assertStartupActive(attempt: StartupAttempt): void {
    if (!this.startupIsActive(attempt)) {
      throw new StartupCancelledError(this.name);
    }
  }

  private cancelStartup(): void {
    const attempt = this.startup;
    if (!attempt || attempt.cancelled) return;
    attempt.cancelled = true;
    attempt.controller.abort();
  }

  private async waitForStartupPoll(
    attempt: StartupAttempt,
    intervalMs: number,
  ): Promise<void> {
    await new Promise<void>((resolve) => {
      const timer = setTimeout(finish, intervalMs);
      const onAbort = () => finish();
      function finish() {
        clearTimeout(timer);
        attempt.controller.signal.removeEventListener("abort", onAbort);
        resolve();
      }
      attempt.controller.signal.addEventListener("abort", onAbort, {
        once: true,
      });
    });
  }

  async ensureRunning(): Promise<void> {
    if (this.isShuttingDown) {
      throw new Error(`${this.name} is shutting down`);
    }
    if (this.failed()) throw this.unavailableError();
    if (this.proc && !this.exited(this.proc)) return;
    if (this.isRestarting) {
      await this.restartPromise;
      if (this.failed()) throw this.unavailableError();
      return await this.ensureRunning();
    }
    await this.start();
  }

  private async start(): Promise<void> {
    const attempt: StartupAttempt = {
      generation: ++this.startupGeneration,
      controller: new AbortController(),
      cancelled: false,
    };
    this.startup = attempt;
    this.lifecycleState = "starting";
    this.isRestarting = true;
    this.restartPromise = (async () => {
      let proc: Bun.Subprocess | null = null;
      let reservation: RuntimeMemoryReservation | null = null;
      const now = Date.now();
      this.crashTimes = this.crashTimes.filter((t) => now - t < 300000);
      const crashCount = this.crashTimes.length;

      try {
        if (crashCount >= 5) {
          this.lifecycleState = "failed";
          this.lifecycle("backend.crash", "error", {
            crashCount,
            crashLimitReached: true,
          });
          void (this.options.onFatal ?? (async () => {}))().catch((err) => {
            this.options.logger.error(
              this.name,
              "Failed to stop manager",
              err as Error,
            );
          });
          throw new CrashLimitReachedError(this.name);
        }

        if (crashCount > 0) {
          const backoffMs = Math.min(1000 * 2 ** (crashCount - 1), 16000);
          this.lifecycle("backend.restart-backoff", "warn", {
            backoffMs,
            crashCount,
          });
          await this.waitForStartupPoll(attempt, backoffMs);
        }

        this.assertStartupActive(attempt);
        this.lifecycle("backend.starting", "info", { crashCount });
        const plan = await this.options.launch();
        this.assertStartupActive(attempt);
        if (plan.runtimeId !== this.options.runtimeId) {
          throw new Error("Backend launch plan has an unexpected runtime ID.");
        }
        reservation = await this.options.memorySafety.reserve({
          runtimeId: plan.runtimeId,
          demand: plan.memoryDemand,
        });
        this.reservation = reservation;
        this.assertStartupActive(attempt);
        proc = await this.options.otel.withSpan(
          "localbase.backend.start",
          internalSpanOptions({ "localbase.backend": this.name }),
          async () => await this.options.start(plan),
        );
        const startedProcess = proc;
        if (!this.startupIsActive(attempt) || this.isShuttingDown) {
          this.expectedStops.add(startedProcess);
          await this.stopProcess(startedProcess);
          throw new StartupCancelledError(this.name);
        }

        this.proc = startedProcess;
        this.startGuardian(startedProcess);
        if (this.proc.stdout && typeof this.proc.stdout !== "number") {
          this.options.logger.pipeStream(this.proc.stdout, this.name);
        }
        if (this.proc.stderr && typeof this.proc.stderr !== "number") {
          this.options.logger.pipeStream(this.proc.stderr, this.name);
        }

        startedProcess.exited.then(() => {
          void this.stopGuardian(startedProcess);
          this.releaseReservation(reservation);
          this.handleCrash(startedProcess);
        });

        await this.options.otel.withSpan(
          "localbase.backend.model_load",
          internalSpanOptions({ "localbase.backend": this.name }),
          async () => await this.waitHealthy(startedProcess, attempt),
        );
        this.assertStartupActive(attempt);
        reservation.materialize();
        this.lifecycle("backend.ready", "info", { pid: startedProcess.pid });
        this.lifecycleState = "running";
      } catch (err) {
        if (err instanceof StartupCancelledError || attempt.cancelled) {
          if (proc && this.proc === proc) {
            await this.stopCurrentProcess();
          } else if (proc) {
            this.expectedStops.add(proc);
            await this.stopProcess(proc);
          }
          this.releaseReservation(reservation);
          throw err instanceof StartupCancelledError
            ? err
            : new StartupCancelledError(this.name);
        }
        if (err instanceof CrashLimitReachedError) throw err;
        if (err instanceof RuntimeMemoryAdmissionError) {
          this.releaseReservation(reservation);
          this.lifecycleState = "idle";
          throw err;
        }
        this.options.logger.event({
          severity: "error",
          eventName: "backend.start-failed",
          category: "runtime",
          component: this.name,
          runtime: this.options.modality,
          message: "Backend startup failed.",
          error: {
            type: err instanceof Error ? err.name : "Error",
            message: err instanceof Error ? err.message : String(err),
          },
        });
        this.crashTimes.push(Date.now());
        await this.stopCurrentProcess();
        this.releaseReservation(reservation);
        this.lifecycleState = "failed";
        throw err;
      } finally {
        if (this.startup === attempt) {
          this.startup = null;
          this.isRestarting = false;
          if (attempt.cancelled && !this.isShuttingDown) {
            this.lifecycleState = "idle";
          }
        }
      }
    })();
    await this.restartPromise;
  }

  private async waitHealthy(
    proc: Bun.Subprocess,
    attempt: StartupAttempt,
  ): Promise<void> {
    const start = Date.now();
    const timeoutMs = this.options.startupTimeoutMs ?? 30000;
    while (Date.now() - start < timeoutMs) {
      this.assertStartupActive(attempt);
      if (this.isShuttingDown) throw new Error(`${this.name} is shutting down`);
      if (this.exited(proc)) {
        const exit =
          proc.signalCode == null
            ? `exit code ${proc.exitCode}`
            : `signal ${proc.signalCode}`;
        this.options.logger.error(
          this.name,
          `Subprocess exited during startup with ${exit}`,
        );
        throw new Error(`${this.name} exited during startup (${exit}).`);
      }
      try {
        const response = await fetch(this.options.healthUrl, {
          signal: AbortSignal.any([
            AbortSignal.timeout(HEALTH_PROBE_TIMEOUT_MS),
            attempt.controller.signal,
          ]),
        });
        if (response.ok) return;
      } catch {
        this.assertStartupActive(attempt);
      }
      await this.waitForStartupPoll(attempt, 200);
    }
    throw new Error("Backend health check timed out.");
  }

  async kill(): Promise<void> {
    if (!this.isShuttingDown) this.lifecycleState = "stopping";
    const startup = this.isRestarting ? this.restartPromise : null;
    this.cancelStartup();
    await this.stopCurrentProcess();
    if (startup) {
      await startup.catch((error) => {
        if (!(error instanceof StartupCancelledError)) throw error;
      });
    }
    if (!this.isShuttingDown) this.lifecycleState = "idle";
  }

  async shutdown(): Promise<void> {
    if (!this.shutdownPromise) {
      this.isShuttingDown = true;
      this.lifecycleState = "stopping";
      this.lifecycle("backend.stopping", "info");
      const startup = this.restartPromise;
      this.shutdownPromise = (async () => {
        this.cancelStartup();
        await this.stopCurrentProcess();
        if (startup) await startup.catch(() => {});
        await this.stopCurrentProcess();
        await this.stopAllGuardians();
        this.lifecycle("backend.stopped", "info");
      })();
    }
    await this.shutdownPromise;
  }

  private async stopCurrentProcess(): Promise<void> {
    const process = this.proc;
    if (!process) return;
    this.expectedStops.add(process);
    this.proc = null;
    this.releaseReservation();
    await this.stopProcess(process);
    await this.stopGuardian(process);
  }

  private releaseReservation(
    reservation: RuntimeMemoryReservation | null = this.reservation,
  ): void {
    if (!reservation) return;
    reservation.release();
    if (this.reservation === reservation) this.reservation = null;
  }

  private startGuardian(proc: Bun.Subprocess): void {
    const guardian = Bun.spawn(guardianProcessCommand(process.pid, proc.pid), {
      stdin: "ignore",
      stdout: "ignore",
      stderr: "ignore",
      detached: true,
    });
    this.guardians.set(proc.pid, guardian);
    guardian.exited.then(() => {
      if (this.guardians.get(proc.pid) === guardian) {
        this.guardians.delete(proc.pid);
      }
    });
  }

  private async stopGuardian(proc: Bun.Subprocess): Promise<void> {
    const guardian = this.guardians.get(proc.pid);
    if (!guardian) return;
    this.guardians.delete(proc.pid);
    await this.stopProcess(guardian);
  }

  private async stopAllGuardians(): Promise<void> {
    const guardians = [...this.guardians.values()];
    this.guardians.clear();
    await Promise.all(
      guardians.map(async (guardian) => await this.stopProcess(guardian)),
    );
  }

  private async stopProcess(proc: Bun.Subprocess): Promise<void> {
    if (this.exited(proc)) return;
    try {
      proc.kill(15);
    } catch {
      return;
    }
    const exitedDuringGrace = await Promise.race([
      proc.exited.then(
        () => true,
        () => true,
      ),
      Bun.sleep(CHILD_STOP_GRACE_MS).then(() => false),
    ]);
    if (exitedDuringGrace || this.exited(proc)) return;
    try {
      proc.kill(9);
    } catch {
      return;
    }
    await proc.exited.catch(() => {});
  }

  private handleCrash(proc: Bun.Subprocess): void {
    if (
      !this.isShuttingDown &&
      !this.expectedStops.has(proc) &&
      this.proc === proc &&
      this.exited(proc) &&
      !this.isRestarting
    ) {
      this.lifecycle("backend.crash", "error", {
        exitCode: proc.exitCode,
        crashCount: this.crashTimes.length + 1,
      });
      this.crashTimes.push(Date.now());
      this.proc = null;
      this.lifecycleState = "starting";
      this.start().catch(() => {});
    }
  }
}
