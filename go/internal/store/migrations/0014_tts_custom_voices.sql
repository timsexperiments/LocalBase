CREATE TABLE `tts_custom_voices` (
	`id` text PRIMARY KEY NOT NULL,
	`name` text NOT NULL,
	`name_key` text NOT NULL,
	`sha256` text NOT NULL,
	`size_bytes` integer NOT NULL,
	`duration_ms` integer NOT NULL,
	`created_at` text NOT NULL,
	`created_by` text NOT NULL,
	`consent_statement` text NOT NULL,
	`consent_attested_at` text NOT NULL
);
--> statement-breakpoint
CREATE UNIQUE INDEX `tts_custom_voices_name_key` ON `tts_custom_voices` (`name_key`);