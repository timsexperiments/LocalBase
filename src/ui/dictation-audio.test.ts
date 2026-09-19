import { expect, test } from "bun:test";
import { encodePcm16Wav, startRecording } from "./dictation-audio";

function browser() {
  const saved = new Map<string, PropertyDescriptor | undefined>();
  const replace = (name: string, value: unknown) => {
    saved.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, value });
  };
  let tracksStopped = 0;
  let permissionCalls = 0;
  let closed = 0;
  let disconnected = 0;
  let timer: (() => void) | undefined;
  let timerMs: number | undefined;
  let recorder: Recorder | undefined;
  let offlineArgs: number[] = [];
  let supported = "audio/webm;codecs=opus";
  let failAt = "";
  let duration = 1;
  const stream = {
    getTracks: () => [
      {
        stop: () => {
          tracksStopped++;
        },
      },
    ],
  };
  const permission = Promise.withResolvers<typeof stream>();
  const decode = Promise.withResolvers<{ duration: number }>();
  let pendingDecode = false;
  let finalChunk = false;
  const decoding = Promise.withResolvers<void>();
  class Recorder {
    static isTypeSupported(type: string) {
      return type === supported;
    }
    state = "inactive";
    mimeType: string;
    ondataavailable: ((event: { data: Blob }) => void) | null = null;
    onstop: (() => void) | null = null;
    onerror: (() => void) | null = null;
    constructor(_stream: unknown, options: { mimeType: string }) {
      if (failAt === "constructor") throw new Error("recorder setup failed");
      this.mimeType = options.mimeType;
      recorder = this;
    }
    start(timeslice: number) {
      expect(timeslice).toBe(250);
      if (failAt === "start") throw new Error("recorder start failed");
      this.state = "recording";
    }
    stop() {
      if (failAt === "stop") throw new Error("recorder stop failed");
      this.state = "inactive";
      // Real recorders deliver final data before the stop event.
      queueMicrotask(() => {
        if (finalChunk)
          this.ondataavailable?.({ data: new Blob(["final audio"]) });
        this.onstop?.();
      });
    }
  }
  class Context {
    async decodeAudioData(bytes: ArrayBuffer) {
      decoding.resolve();
      expect(bytes.byteLength).toBeGreaterThan(0);
      if (failAt === "decode") throw new Error("decode failed");
      return pendingDecode ? decode.promise : { duration };
    }
    async close() {
      closed++;
    }
  }
  class Offline {
    destination = {};
    constructor(channels: number, length: number, rate: number) {
      offlineArgs = [channels, length, rate];
    }
    createBufferSource() {
      return {
        buffer: null,
        connect() {},
        start() {},
        disconnect() {
          disconnected++;
        },
      };
    }
    async startRendering() {
      if (failAt === "render") throw new Error("render failed");
      return {
        getChannelData: (channel: number) => {
          expect(channel).toBe(0);
          return new Float32Array(offlineArgs[1]);
        },
      };
    }
  }
  replace("navigator", {
    mediaDevices: {
      getUserMedia: (constraints: unknown) => {
        permissionCalls++;
        expect(constraints).toEqual({ audio: true });
        return permission.promise;
      },
    },
  });
  replace("MediaRecorder", Recorder);
  replace("AudioContext", Context);
  replace("OfflineAudioContext", Offline);
  replace("setTimeout", (callback: () => void, ms: number) => {
    timer = callback;
    timerMs = ms;
    return 1;
  });
  replace("clearTimeout", () => {
    timer = undefined;
  });
  return {
    permission,
    decode,
    decoding: decoding.promise,
    deliverFinalChunk() {
      finalChunk = true;
    },
    allow() {
      permission.resolve(stream);
    },
    rejectPermission() {
      permission.reject(
        new DOMException("Permission denied", "NotAllowedError"),
      );
    },
    format(type: string) {
      supported = type;
    },
    fail(stage: string) {
      failAt = stage;
    },
    holdDecode() {
      pendingDecode = true;
    },
    duration(seconds: number) {
      duration = seconds;
    },
    recorder() {
      if (!recorder) throw new Error("Recorder not created.");
      return recorder;
    },
    chunk(size = 3) {
      this.recorder().ondataavailable?.({
        data: new Blob([new Uint8Array(size)]),
      });
    },
    advanceLimit() {
      expect(timerMs).toBe(60_000);
      if (!timer) throw new Error("No recording timer.");
      timer();
    },
    get tracksStopped() {
      return tracksStopped;
    },
    get permissionCalls() {
      return permissionCalls;
    },
    get closed() {
      return closed;
    },
    get disconnected() {
      return disconnected;
    },
    get offlineArgs() {
      return offlineArgs;
    },
    get hasTimer() {
      return !!timer;
    },
    restore() {
      for (const [name, descriptor] of saved) {
        if (descriptor) Object.defineProperty(globalThis, name, descriptor);
        else Reflect.deleteProperty(globalThis, name);
      }
    },
  };
}

test("PCM16 WAV has correct headers, clipped samples and bounded duration", async () => {
  const blob = encodePcm16Wav(
    new Float32Array([-2, -1, -0.5, 0, 0.5, 1, 2, NaN]),
  );
  const buffer = await blob.arrayBuffer();
  const view = new DataView(buffer);
  expect(blob.type).toBe("audio/wav");
  expect(new TextDecoder().decode(buffer.slice(0, 4))).toBe("RIFF");
  expect(new TextDecoder().decode(buffer.slice(8, 16))).toBe("WAVEfmt ");
  expect(view.getUint32(4, true)).toBe(buffer.byteLength - 8);
  expect(view.getUint16(20, true)).toBe(1);
  expect(view.getUint16(22, true)).toBe(1);
  expect(view.getUint32(24, true)).toBe(16000);
  expect(view.getUint32(28, true)).toBe(32000);
  expect(view.getUint16(32, true)).toBe(2);
  expect(view.getUint16(34, true)).toBe(16);
  expect(new TextDecoder().decode(buffer.slice(36, 40))).toBe("data");
  expect(view.getUint32(40, true)).toBe(16);
  expect(
    Array.from({ length: 8 }, (_, i) => view.getInt16(44 + i * 2, true)),
  ).toEqual([-32768, -32768, -16384, 0, 16384, 32767, 32767, 0]);
  expect(() => encodePcm16Wav(new Float32Array())).toThrow();
  expect(() => encodePcm16Wav(new Float32Array(960001))).toThrow();
});

test.serial(
  "includes final data delivered asynchronously after stop",
  async () => {
    const fake = browser();
    try {
      fake.allow();
      fake.deliverFinalChunk();
      const recording = await startRecording({
        signal: new AbortController().signal,
      });
      recording.stop();
      expect((await recording.finished).type).toBe("audio/wav");
      expect(fake.tracksStopped).toBe(1);
    } finally {
      fake.restore();
    }
  },
);

test.serial(
  "cancellation during decoding rejects promptly and closes the audio context",
  async () => {
    const fake = browser();
    try {
      fake.allow();
      fake.holdDecode();
      const recording = await startRecording({
        signal: new AbortController().signal,
      });
      fake.chunk();
      recording.stop();
      await fake.decoding;
      recording.cancel();
      await expect(recording.finished).rejects.toMatchObject({
        name: "AbortError",
      });
      expect(fake.closed).toBe(1);
      expect(fake.tracksStopped).toBe(1);
      fake.decode.resolve({ duration: 1 });
      await fake.decode.promise;
      expect(fake.offlineArgs).toEqual([]);
    } finally {
      fake.restore();
    }
  },
);

for (const mime of ["audio/webm;codecs=opus", "audio/webm", "audio/mp4"]) {
  test.serial(
    `records ${mime}, releases capture and renders mono 16 kHz WAV`,
    async () => {
      const fake = browser();
      try {
        fake.format(mime);
        fake.allow();
        const recording = await startRecording({
          signal: new AbortController().signal,
        });
        expect(fake.recorder().mimeType).toBe(mime);
        fake.chunk();
        recording.stop();
        expect(fake.tracksStopped).toBeGreaterThan(0);
        recording.stop();
        const wav = await recording.finished;
        expect(wav.type).toBe("audio/wav");
        expect(wav.size).toBe(44 + 16000 * 2);
        expect(fake.offlineArgs).toEqual([1, 16000, 16000]);
        expect(fake.closed).toBe(1);
        expect(fake.disconnected).toBe(1);
        expect(fake.hasTimer).toBe(false);
      } finally {
        fake.restore();
      }
    },
  );
}

test.serial(
  "aborts permission promptly and releases tracks granted after cancellation",
  async () => {
    const fake = browser();
    try {
      const abort = new AbortController();
      const pending = startRecording({ signal: abort.signal });
      abort.abort();
      await expect(pending).rejects.toMatchObject({ name: "AbortError" });
      fake.allow();
      await fake.permission.promise;
      expect(fake.tracksStopped).toBe(1);
      await expect(
        startRecording({ signal: abort.signal }),
      ).rejects.toMatchObject({ name: "AbortError" });
      expect(fake.permissionCalls).toBe(1);
    } finally {
      fake.restore();
    }
  },
);

test.serial(
  "permission denial and unsupported formats never start capture",
  async () => {
    const fake = browser();
    try {
      const pending = startRecording({ signal: new AbortController().signal });
      fake.rejectPermission();
      await expect(pending).rejects.toMatchObject({ name: "NotAllowedError" });
      fake.format("unsupported");
      await expect(
        startRecording({ signal: new AbortController().signal }),
      ).rejects.toThrow("cannot record");
      expect(fake.permissionCalls).toBe(1);
      expect(fake.hasTimer).toBe(false);
    } finally {
      fake.restore();
    }
  },
);

for (const stage of ["constructor", "start", "stop", "decode", "render"]) {
  test.serial(`cleans up after ${stage} failure`, async () => {
    const fake = browser();
    try {
      fake.fail(stage);
      fake.allow();
      const pending = startRecording({ signal: new AbortController().signal });
      if (stage === "constructor" || stage === "start")
        await expect(pending).rejects.toThrow();
      else {
        const recording = await pending;
        fake.chunk();
        recording.stop();
        await expect(recording.finished).rejects.toThrow();
      }
      expect(fake.tracksStopped).toBeGreaterThan(0);
      expect(fake.hasTimer).toBe(false);
      if (stage === "decode" || stage === "render") expect(fake.closed).toBe(1);
      if (stage === "render") expect(fake.disconnected).toBe(1);
    } finally {
      fake.restore();
    }
  });
}

test.serial(
  "timer resolves finished automatically and caps decoded encoder padding",
  async () => {
    const fake = browser();
    try {
      fake.allow();
      fake.duration(60.1);
      const recording = await startRecording({
        signal: new AbortController().signal,
      });
      fake.chunk();
      fake.advanceLimit();
      expect((await recording.finished).size).toBe(44 + 960000 * 2);
      expect(fake.offlineArgs).toEqual([1, 960000, 16000]);
      expect(fake.tracksStopped).toBeGreaterThan(0);
    } finally {
      fake.restore();
    }
  },
);

for (const failure of ["size", "error", "empty", "cancel", "abort"]) {
  test.serial(
    `${failure} rejects finished and releases microphone without awaiting stop`,
    async () => {
      const fake = browser();
      try {
        fake.allow();
        const abort = new AbortController();
        const recording = await startRecording({ signal: abort.signal });
        if (failure === "size") {
          fake.chunk(16 * 1024 * 1024);
          fake.chunk(1);
        }
        if (failure === "error") fake.recorder().onerror?.();
        if (failure === "empty") recording.stop();
        if (failure === "cancel") recording.cancel();
        if (failure === "abort") abort.abort();
        await expect(recording.finished).rejects.toThrow();
        expect(fake.tracksStopped).toBeGreaterThan(0);
        expect(fake.hasTimer).toBe(false);
        recording.cancel();
        recording.stop();
        expect(fake.closed).toBe(0);
      } finally {
        fake.restore();
      }
    },
  );
}
