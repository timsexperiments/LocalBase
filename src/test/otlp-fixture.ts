type ProtobufField = { wire: number; value: Uint8Array | number };

function protobufFields(bytes: Uint8Array): Map<number, ProtobufField[]> {
  const fields = new Map<number, ProtobufField[]>();
  let offset = 0;
  const varint = (): number => {
    let value = 0;
    let shift = 0;
    while (offset < bytes.length) {
      const byte = bytes[offset++];
      value += (byte & 0x7f) * 2 ** shift;
      if ((byte & 0x80) === 0) return value;
      shift += 7;
    }
    throw new Error("Truncated protobuf varint.");
  };
  while (offset < bytes.length) {
    const tag = varint();
    const field = Math.floor(tag / 8);
    const wire = tag & 7;
    let value: Uint8Array | number;
    if (wire === 0) {
      value = varint();
    } else if (wire === 1) {
      value = bytes.slice(offset, offset + 8);
      offset += 8;
    } else if (wire === 2) {
      const length = varint();
      value = bytes.slice(offset, offset + length);
      offset += length;
    } else if (wire === 5) {
      value = bytes.slice(offset, offset + 4);
      offset += 4;
    } else {
      throw new Error(`Unsupported protobuf wire type ${wire}.`);
    }
    const existing = fields.get(field) ?? [];
    existing.push({ wire, value });
    fields.set(field, existing);
  }
  return fields;
}

function bytes(field: ProtobufField | undefined): Uint8Array | undefined {
  return field?.value instanceof Uint8Array ? field.value : undefined;
}

function number(field: ProtobufField | undefined): number | undefined {
  return typeof field?.value === "number" ? field.value : undefined;
}

function text(field: ProtobufField | undefined): string {
  const value = bytes(field);
  return value ? new TextDecoder().decode(value) : "";
}

function hex(field: ProtobufField | undefined): string {
  const value = bytes(field);
  return value ? Buffer.from(value).toString("hex") : "";
}

function anyValue(value: Uint8Array): string | number | boolean | undefined {
  const fields = protobufFields(value);
  const stringValue = fields.get(1)?.[0];
  if (stringValue) return text(stringValue);
  const boolValue = number(fields.get(2)?.[0]);
  if (boolValue !== undefined) return boolValue !== 0;
  const intValue = number(fields.get(3)?.[0]);
  if (intValue !== undefined) return intValue;
  const doubleValue = bytes(fields.get(4)?.[0]);
  if (doubleValue?.byteLength === 8) {
    return new DataView(
      doubleValue.buffer,
      doubleValue.byteOffset,
      doubleValue.byteLength,
    ).getFloat64(0, true);
  }
  return undefined;
}

export type DecodedOtlpSpan = {
  traceId: string;
  spanId: string;
  parentSpanId: string;
  name: string;
  statusCode: number;
  statusMessage: string;
  attributes: Record<string, string | number | boolean>;
};

export function decodeOtlpTraceSpans(payload: Uint8Array): DecodedOtlpSpan[] {
  const spans: DecodedOtlpSpan[] = [];
  for (const resourceSpans of protobufFields(payload).get(1) ?? []) {
    for (const scopeSpans of protobufFields(bytes(resourceSpans)!).get(2) ??
      []) {
      for (const span of protobufFields(bytes(scopeSpans)!).get(2) ?? []) {
        const fields = protobufFields(bytes(span)!);
        const attributes: Record<string, string | number | boolean> = {};
        for (const attribute of fields.get(9) ?? []) {
          const attributeFields = protobufFields(bytes(attribute)!);
          const key = text(attributeFields.get(1)?.[0]);
          const encodedValue = bytes(attributeFields.get(2)?.[0]);
          const value = encodedValue ? anyValue(encodedValue) : undefined;
          if (key && value !== undefined) attributes[key] = value;
        }
        const status = bytes(fields.get(15)?.[0]);
        const statusFields = status ? protobufFields(status) : new Map();
        spans.push({
          traceId: hex(fields.get(1)?.[0]),
          spanId: hex(fields.get(2)?.[0]),
          parentSpanId: hex(fields.get(4)?.[0]),
          name: text(fields.get(5)?.[0]),
          statusCode: number(statusFields.get(3)?.[0]) ?? 0,
          statusMessage: text(statusFields.get(2)?.[0]),
          attributes,
        });
      }
    }
  }
  return spans;
}

export type DecodedOtlpLog = {
  traceId: string;
  spanId: string;
  body: string;
  attributes: Record<string, string | number | boolean>;
};

export function decodeOtlpLogs(payload: Uint8Array): DecodedOtlpLog[] {
  const records: DecodedOtlpLog[] = [];
  for (const resourceLogs of protobufFields(payload).get(1) ?? []) {
    for (const scopeLogs of protobufFields(bytes(resourceLogs)!).get(2) ?? []) {
      for (const record of protobufFields(bytes(scopeLogs)!).get(2) ?? []) {
        const fields = protobufFields(bytes(record)!);
        const attributes: Record<string, string | number | boolean> = {};
        for (const attribute of fields.get(6) ?? []) {
          const attributeFields = protobufFields(bytes(attribute)!);
          const key = text(attributeFields.get(1)?.[0]);
          const encodedValue = bytes(attributeFields.get(2)?.[0]);
          const value = encodedValue ? anyValue(encodedValue) : undefined;
          if (key && value !== undefined) attributes[key] = value;
        }
        const encodedBody = bytes(fields.get(5)?.[0]);
        records.push({
          traceId: hex(fields.get(9)?.[0]),
          spanId: hex(fields.get(10)?.[0]),
          body: encodedBody ? String(anyValue(encodedBody) ?? "") : "",
          attributes,
        });
      }
    }
  }
  return records;
}
