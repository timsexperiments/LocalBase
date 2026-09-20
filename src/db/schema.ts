import { integer, real, sqliteTable, text } from "drizzle-orm/sqlite-core";

export const configTable = sqliteTable("config", {
  id: text("id").primaryKey(),
  root: text("root").notNull(),
  gatewayHost: text("gateway_host").default("127.0.0.1").notNull(),
  gatewayPort: integer("gateway_port").default(2273).notNull(),
  host: text("host").notNull(),
  port: integer("port").notNull(),
  ctxSize: integer("ctx_size").notNull(),
  sttHost: text("stt_host").notNull(),
  sttPort: integer("stt_port").notNull(),
  selectedLlmModels: text("selected_llm_models").notNull(),
  selectedSttModels: text("selected_stt_models").notNull(),
  selectedTtsModels: text("selected_tts_models").notNull(),
  selectedImageModels: text("selected_image_models").notNull(),
  selectedVideoModels: text("selected_video_models").notNull(),
  activeLlmModel: text("active_llm_model").notNull(),
  activeSttModel: text("active_stt_model").notNull(),
  activeTtsModel: text("active_tts_model").notNull(),
  activeImageModel: text("active_image_model").notNull(),
  activeVideoModel: text("active_video_model").notNull(),
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

export const configActivationTable = sqliteTable("config_activation", {
  id: text("id").primaryKey(),
  pendingStaticConfig: text("pending_static_config").notNull(),
});

export const apiKeysTable = sqliteTable("api_keys", {
  id: text("id").primaryKey(),
  name: text("name").notNull(),
  prefix: text("prefix").notNull(),
  keyHash: text("key_hash").notNull(),
  scopes: text("scopes").default("[]").notNull(),
  createdAt: text("created_at").notNull(),
  lastRotatedAt: text("last_rotated_at").notNull(),
  expiresAt: text("expires_at"),
  revokedAt: text("revoked_at"),
});
