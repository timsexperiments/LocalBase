CREATE TABLE `api_keys` (
	`id` text PRIMARY KEY NOT NULL,
	`name` text NOT NULL,
	`prefix` text NOT NULL,
	`key_hash` text NOT NULL,
	`created_at` text NOT NULL,
	`last_rotated_at` text NOT NULL,
	`expires_at` text,
	`revoked_at` text
);
--> statement-breakpoint
CREATE TABLE `config` (
	`id` text PRIMARY KEY NOT NULL,
	`root` text NOT NULL,
	`host` text NOT NULL,
	`port` integer NOT NULL,
	`ctx_size` integer NOT NULL,
	`stt_host` text NOT NULL,
	`stt_port` integer NOT NULL,
	`selected_llm_models` text NOT NULL,
	`selected_stt_models` text NOT NULL,
	`selected_image_models` text NOT NULL,
	`active_llm_model` text NOT NULL,
	`active_stt_model` text NOT NULL,
	`active_image_model` text NOT NULL,
	`hf_token` text NOT NULL,
	`parallel` text DEFAULT 'auto' NOT NULL,
	`otel_endpoint` text DEFAULT '' NOT NULL,
	`otel_headers` text DEFAULT '' NOT NULL,
	`otel_sample_ratio` integer DEFAULT 100 NOT NULL,
	`memory_system_reserve_percent` real NOT NULL,
	`memory_system_reserve_minimum_gb` real NOT NULL,
	`memory_accelerator_reserve_percent` real NOT NULL,
	`memory_accelerator_reserve_minimum_gb` real NOT NULL
);
