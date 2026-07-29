ALTER TABLE `config` ADD `otel_endpoint` text DEFAULT '' NOT NULL;--> statement-breakpoint
ALTER TABLE `config` ADD `otel_headers` text DEFAULT '' NOT NULL;--> statement-breakpoint
ALTER TABLE `config` ADD `otel_sample_ratio` integer DEFAULT 100 NOT NULL;