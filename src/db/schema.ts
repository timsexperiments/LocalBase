import {
  integer,
  primaryKey,
  real,
  sqliteTable,
  text,
  uniqueIndex,
} from "drizzle-orm/sqlite-core";

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

export const authRolesTable = sqliteTable(
  "auth_roles",
  {
    id: text("id").primaryKey(),
    name: text("name").notNull(),
    description: text("description").notNull(),
    createdAt: text("created_at").notNull(),
    updatedAt: text("updated_at").notNull(),
  },
  (table) => [uniqueIndex("auth_roles_name_unique").on(table.name)],
);

export const authRolePermissionsTable = sqliteTable(
  "auth_role_permissions",
  {
    roleId: text("role_id")
      .notNull()
      .references(() => authRolesTable.id, { onDelete: "cascade" }),
    permission: text("permission").notNull(),
  },
  (table) => [primaryKey({ columns: [table.roleId, table.permission] })],
);

export const authUsersTable = sqliteTable("auth_users", {
  id: text("id").primaryKey(),
  status: text("status").notNull(),
  createdAt: text("created_at").notNull(),
  updatedAt: text("updated_at").notNull(),
});

export const authUserEmailsTable = sqliteTable(
  "auth_user_emails",
  {
    userId: text("user_id")
      .primaryKey()
      .references(() => authUsersTable.id, { onDelete: "cascade" }),
    email: text("email").notNull(),
  },
  (table) => [uniqueIndex("auth_user_emails_email_unique").on(table.email)],
);

export const authIdentitiesTable = sqliteTable(
  "auth_identities",
  {
    id: text("id").primaryKey(),
    userId: text("user_id")
      .notNull()
      .references(() => authUsersTable.id, { onDelete: "cascade" }),
    issuer: text("issuer").notNull(),
    subject: text("subject").notNull(),
    verifiedEmail: text("verified_email"),
    createdAt: text("created_at").notNull(),
    lastSeenAt: text("last_seen_at").notNull(),
  },
  (table) => [
    uniqueIndex("auth_identities_user_id_unique").on(table.userId),
    uniqueIndex("auth_identities_issuer_subject_unique").on(
      table.issuer,
      table.subject,
    ),
  ],
);

export const authUserRolesTable = sqliteTable(
  "auth_user_roles",
  {
    userId: text("user_id")
      .notNull()
      .references(() => authUsersTable.id, { onDelete: "cascade" }),
    roleId: text("role_id")
      .notNull()
      .references(() => authRolesTable.id, { onDelete: "cascade" }),
  },
  (table) => [primaryKey({ columns: [table.userId, table.roleId] })],
);

export const authSubjectRoleBindingsTable = sqliteTable(
  "auth_subject_role_bindings",
  {
    id: text("id").primaryKey(),
    roleId: text("role_id")
      .notNull()
      .references(() => authRolesTable.id, { onDelete: "cascade" }),
    issuer: text("issuer").notNull(),
    subject: text("subject").notNull(),
  },
  (table) => [
    uniqueIndex("auth_subject_role_bindings_unique").on(
      table.roleId,
      table.issuer,
      table.subject,
    ),
  ],
);

export const authEmailRoleBindingsTable = sqliteTable(
  "auth_email_role_bindings",
  {
    id: text("id").primaryKey(),
    roleId: text("role_id")
      .notNull()
      .references(() => authRolesTable.id, { onDelete: "cascade" }),
    email: text("email").notNull(),
  },
  (table) => [
    uniqueIndex("auth_email_role_bindings_unique").on(
      table.roleId,
      table.email,
    ),
  ],
);

export const authDomainRoleBindingsTable = sqliteTable(
  "auth_domain_role_bindings",
  {
    id: text("id").primaryKey(),
    roleId: text("role_id")
      .notNull()
      .references(() => authRolesTable.id, { onDelete: "cascade" }),
    domain: text("domain").notNull(),
  },
  (table) => [
    uniqueIndex("auth_domain_role_bindings_unique").on(
      table.roleId,
      table.domain,
    ),
  ],
);

export const authSettingsTable = sqliteTable("auth_settings", {
  id: text("id").primaryKey(),
  defaultRoleId: text("default_role_id").references(() => authRolesTable.id, {
    onDelete: "set null",
  }),
  updatedAt: text("updated_at").notNull(),
});
