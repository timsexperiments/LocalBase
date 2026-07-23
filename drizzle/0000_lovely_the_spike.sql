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
	`llm_models_dir` text NOT NULL,
	`stt_models_dir` text NOT NULL,
	`image_models_dir` text NOT NULL,
	`runtime_backend` text NOT NULL,
	`stt_backend` text NOT NULL,
	`host` text NOT NULL,
	`port` integer NOT NULL,
	`ctx_size` integer NOT NULL,
	`stt_host` text NOT NULL,
	`stt_port` integer NOT NULL,
	`startup_on_boot` integer NOT NULL,
	`selected_llm_models` text NOT NULL,
	`selected_stt_models` text NOT NULL,
	`selected_image_models` text NOT NULL,
	`active_llm_model` text NOT NULL,
	`active_stt_model` text NOT NULL,
	`active_image_model` text NOT NULL,
	`system_prompt` text NOT NULL,
	`hf_token` text NOT NULL,
	`parallel` text DEFAULT 'auto' NOT NULL
);
