import type { RuntimeModality } from "./modality";

export type ModalityAdmission<Value> = Readonly<{
  modality: RuntimeModality;
  value: Value;
  onDetach: (callback: () => void) => void;
  markResponseStarted: () => void;
  release: () => void;
}>;

/** Tracks request work admitted to one replaceable backend. */
export class ModalityAdmissionBarrier {
  private accepting: boolean;
  private active = 0;
  private idle = Promise.resolve();
  private resolveIdle: (() => void) | undefined;
  private readonly pendingResponses = new Set<() => void>();

  constructor(
    private readonly modality: RuntimeModality,
    configured: boolean,
  ) {
    this.accepting = configured;
  }

  attach(): void {
    this.accepting = true;
  }

  detach(): void {
    this.accepting = false;
  }

  acquire<Value>(value: Value): ModalityAdmission<Value> | undefined {
    if (!this.accepting) return undefined;
    if (this.active === 0) {
      this.idle = new Promise<void>((resolve) => {
        this.resolveIdle = resolve;
      });
    }
    this.active += 1;
    let released = false;
    let responseStarted = false;
    let onDetach: (() => void) | undefined;
    return {
      modality: this.modality,
      value,
      onDetach: (callback) => {
        if (released || responseStarted) return;
        onDetach = callback;
        this.pendingResponses.add(callback);
      },
      markResponseStarted: () => {
        if (responseStarted) return;
        responseStarted = true;
        if (onDetach) this.pendingResponses.delete(onDetach);
      },
      release: () => {
        if (released) return;
        released = true;
        if (onDetach) this.pendingResponses.delete(onDetach);
        this.active -= 1;
        if (this.active === 0) {
          this.resolveIdle?.();
          this.resolveIdle = undefined;
        }
      },
    };
  }

  async drain(): Promise<void> {
    this.detach();
    for (const cancelPendingResponse of this.pendingResponses) {
      cancelPendingResponse();
    }
    await this.idle;
  }
}
