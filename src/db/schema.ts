import { integer, real, sqliteTable, text } from "drizzle-orm/sqlite-core";

export const configTable = sqliteTable("config", {
  id: text("id").primaryKey(),
  root: text("root").notNull(),
  host: text("host").notNull(),
  port: integer("port").notNull(),
  ctxSize: integer("ctx_size").notNull(),
  sttHost: text("stt_host").notNull(),
  sttPort: integer("stt_port").notNull(),
  selectedLlmModels: text("selected_llm_models").notNull(),
  selectedSttModels: text("selected_stt_models").notNull(),
  selectedImageModels: text("selected_image_models").notNull(),
  activeLlmModel: text("active_llm_model").notNull(),
  activeSttModel: text("active_stt_model").notNull(),
  activeImageModel: text("active_image_model").notNull(),
  hfToken: text("hf_token").notNull(),
  parallel: text("parallel").default("auto").notNull(),
  otelEndpoint: text("otel_endpoint").default("").notNull(),
  otelHeaders: text("otel_headers").default("").notNull(),
  otelSampleRatio: integer("otel_sample_ratio").default(100).notNull(),
  memorySystemReservePercent: real("memory_system_reserve_percent").notNull(),
  memorySystemReserveMinimumGb: real(
    "memory_system_reserve_minimum_gb",
  ).notNull(),
  memoryAcceleratorReservePercent: real(
    "memory_accelerator_reserve_percent",
  ).notNull(),
  memoryAcceleratorReserveMinimumGb: real(
    "memory_accelerator_reserve_minimum_gb",
  ).notNull(),
});

export const apiKeysTable = sqliteTable("api_keys", {
  id: text("id").primaryKey(),
  name: text("name").notNull(),
  prefix: text("prefix").notNull(),
  keyHash: text("key_hash").notNull(),
  createdAt: text("created_at").notNull(),
  lastRotatedAt: text("last_rotated_at").notNull(),
  expiresAt: text("expires_at"),
  revokedAt: text("revoked_at"),
});
