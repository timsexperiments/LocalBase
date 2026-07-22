import { integer, sqliteTable, text } from "drizzle-orm/sqlite-core";

// Nullable legacy columns remain nullable at the database layer. Config reads
// validate their semantic requirements with Zod before returning them.
export const configTable = sqliteTable("config", {
  id: text("id").primaryKey(),
  root: text("root").notNull(),
  llmModelsDir: text("llm_models_dir").notNull(),
  sttModelsDir: text("stt_models_dir").notNull(),
  imageModelsDir: text("image_models_dir"),
  runtimeBackend: text("runtime_backend").notNull(),
  sttBackend: text("stt_backend").notNull(),
  host: text("host").notNull(),
  port: integer("port").notNull(),
  ctxSize: integer("ctx_size").notNull(),
  sttHost: text("stt_host").notNull(),
  sttPort: integer("stt_port").notNull(),
  startupOnBoot: integer("startup_on_boot").notNull(),
  selectedLlmModels: text("selected_llm_models").notNull(),
  selectedSttModels: text("selected_stt_models").notNull(),
  selectedImageModels: text("selected_image_models"),
  activeLlmModel: text("active_llm_model").notNull(),
  activeSttModel: text("active_stt_model").notNull(),
  activeImageModel: text("active_image_model"),
  systemPrompt: text("system_prompt"),
  hfToken: text("hf_token"),
  parallel: text("parallel").default("auto").notNull(),
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
