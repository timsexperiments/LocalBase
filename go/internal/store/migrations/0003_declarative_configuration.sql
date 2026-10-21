CREATE TABLE `config_activation` (
	`id` text PRIMARY KEY NOT NULL,
	`pending_static_config` text NOT NULL
);
--> statement-breakpoint
ALTER TABLE `config` ADD `gateway_host` text DEFAULT '127.0.0.1' NOT NULL;--> statement-breakpoint
ALTER TABLE `config` ADD `gateway_port` integer DEFAULT 2273 NOT NULL;