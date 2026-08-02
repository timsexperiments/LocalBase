import { constants } from "node:fs";
import { dirname, isAbsolute, resolve } from "node:path";

type NativeSymbols = {
  openat(directory: number, path: Buffer, flags: number, mode: number): number;
  close(file: number): number;
  write(file: number, data: Uint8Array, length: number): bigint;
  fsync(file: number): number;
  fchmod(file: number, mode: number): number;
  linkat(
    oldDirectory: number,
    oldPath: Buffer,
    newDirectory: number,
    newPath: Buffer,
    flags: number,
  ): number;
  unlinkat(directory: number, path: Buffer, flags: number): number;
};

type NativeFs = { symbols: NativeSymbols };

export type SecureFilePublicationTestHooks = {
  afterCreate?: (file: number, name: string) => void;
  beforeWrite?: () => void;
};

let testHooks: SecureFilePublicationTestHooks | undefined;

export function setSecureFilePublicationTestHooksForTests(
  hooks?: SecureFilePublicationTestHooks,
): void {
  testHooks = hooks;
}

const AT_FDCWD = -100;
const READ_DIRECTORY_FLAGS =
  constants.O_RDONLY |
  (constants.O_DIRECTORY ?? 0) |
  (constants.O_NOFOLLOW ?? 0);
const EXCLUSIVE_FILE_FLAGS =
  constants.O_WRONLY |
  constants.O_CREAT |
  constants.O_EXCL |
  (constants.O_NOFOLLOW ?? 0);

let nativeFs: NativeFs | undefined;

function native(): NativeSymbols {
  if (nativeFs) return nativeFs.symbols;
  const { dlopen } = require("bun:ffi") as typeof import("bun:ffi");
  const library =
    process.platform === "darwin" ? "/usr/lib/libSystem.B.dylib" : "libc.so.6";
  nativeFs = dlopen(library, {
    openat: {
      args: ["i32", "cstring", "i32", "i32"],
      returns: "i32",
    },
    close: { args: ["i32"], returns: "i32" },
    write: { args: ["i32", "ptr", "usize"], returns: "i64" },
    fsync: { args: ["i32"], returns: "i32" },
    fchmod: { args: ["i32", "i32"], returns: "i32" },
    linkat: {
      args: ["i32", "cstring", "i32", "cstring", "i32"],
      returns: "i32",
    },
    unlinkat: { args: ["i32", "cstring", "i32"], returns: "i32" },
  }) as unknown as NativeFs;
  return nativeFs.symbols;
}

function cstring(value: string): Buffer {
  return Buffer.from(`${value}\0`);
}

function check(result: number, operation: string): void {
  if (result < 0) throw new Error(`Secure diagnostics ${operation} failed.`);
}

function pathComponents(path: string): string[] {
  let absolute = resolve(path);
  if (process.platform === "darwin") {
    for (const alias of ["/var", "/tmp", "/etc"]) {
      if (absolute === alias || absolute.startsWith(`${alias}/`)) {
        absolute = `/private${absolute}`;
        break;
      }
    }
  }
  if (!isAbsolute(absolute))
    throw new Error("Secure output parent is invalid.");
  return absolute.split("/").filter(Boolean);
}

export type SecureDirectory = {
  createExclusiveFile(name: string, mode: number): number;
  write(file: number, data: Uint8Array): void;
  sync(file: number): void;
  closeFile(file: number): void;
  publish(name: string, destination: string): void;
  remove(name: string): void;
  syncDirectory(): void;
  close(): void;
};

export function openSecureDirectory(path: string): SecureDirectory {
  const symbols = native();
  let directory = symbols.openat(
    AT_FDCWD,
    cstring("/"),
    READ_DIRECTORY_FLAGS,
    0,
  );
  check(directory, "directory open");
  try {
    for (const component of pathComponents(path)) {
      const next = symbols.openat(
        directory,
        cstring(component),
        READ_DIRECTORY_FLAGS,
        0,
      );
      check(next, "directory open");
      check(symbols.close(directory), "directory close");
      directory = next;
    }
  } catch (error) {
    symbols.close(directory);
    throw error;
  }

  return {
    createExclusiveFile(name, mode) {
      const file = symbols.openat(
        directory,
        cstring(name),
        EXCLUSIVE_FILE_FLAGS,
        mode,
      );
      check(file, "temporary file creation");
      try {
        check(symbols.fchmod(file, mode), "temporary file permission setup");
      } catch (error) {
        symbols.close(file);
        symbols.unlinkat(directory, cstring(name), 0);
        throw error;
      }
      try {
        testHooks?.afterCreate?.(file, name);
      } catch (error) {
        symbols.close(file);
        symbols.unlinkat(directory, cstring(name), 0);
        throw error;
      }
      return file;
    },
    write(file, data) {
      testHooks?.beforeWrite?.();
      let offset = 0;
      while (offset < data.byteLength) {
        const written = symbols.write(
          file,
          data.subarray(offset),
          data.byteLength - offset,
        );
        if (written <= 0n) throw new Error("Secure diagnostics write failed.");
        offset += Number(written);
      }
    },
    sync(file) {
      check(symbols.fsync(file), "file sync");
    },
    closeFile(file) {
      check(symbols.close(file), "file close");
    },
    publish(name, destination) {
      check(
        symbols.linkat(
          directory,
          cstring(name),
          directory,
          cstring(destination),
          0,
        ),
        "archive publication; output already exists",
      );
    },
    remove(name) {
      symbols.unlinkat(directory, cstring(name), 0);
    },
    syncDirectory() {
      check(symbols.fsync(directory), "directory sync");
    },
    close() {
      check(symbols.close(directory), "directory close");
      directory = -1;
    },
  };
}

export function outputParent(path: string): string {
  return dirname(resolve(path));
}
