CREATE TABLE `auth_domain_role_bindings` (
	`id` text PRIMARY KEY NOT NULL,
	`role_id` text NOT NULL,
	`domain` text NOT NULL,
	FOREIGN KEY (`role_id`) REFERENCES `auth_roles`(`id`) ON UPDATE no action ON DELETE cascade
);
--> statement-breakpoint
CREATE UNIQUE INDEX `auth_domain_role_bindings_unique` ON `auth_domain_role_bindings` (`role_id`,`domain`);--> statement-breakpoint
CREATE TABLE `auth_email_role_bindings` (
	`id` text PRIMARY KEY NOT NULL,
	`role_id` text NOT NULL,
	`email` text NOT NULL,
	FOREIGN KEY (`role_id`) REFERENCES `auth_roles`(`id`) ON UPDATE no action ON DELETE cascade
);
--> statement-breakpoint
CREATE UNIQUE INDEX `auth_email_role_bindings_unique` ON `auth_email_role_bindings` (`role_id`,`email`);--> statement-breakpoint
CREATE TABLE `auth_identities` (
	`id` text PRIMARY KEY NOT NULL,
	`user_id` text NOT NULL,
	`issuer` text NOT NULL,
	`subject` text NOT NULL,
	`verified_email` text,
	`created_at` text NOT NULL,
	`last_seen_at` text NOT NULL,
	FOREIGN KEY (`user_id`) REFERENCES `auth_users`(`id`) ON UPDATE no action ON DELETE cascade
);
--> statement-breakpoint
CREATE UNIQUE INDEX `auth_identities_issuer_subject_unique` ON `auth_identities` (`issuer`,`subject`);--> statement-breakpoint
CREATE TABLE `auth_role_permissions` (
	`role_id` text NOT NULL,
	`permission` text NOT NULL,
	PRIMARY KEY(`role_id`, `permission`),
	FOREIGN KEY (`role_id`) REFERENCES `auth_roles`(`id`) ON UPDATE no action ON DELETE cascade
);
--> statement-breakpoint
CREATE TABLE `auth_roles` (
	`id` text PRIMARY KEY NOT NULL,
	`name` text NOT NULL,
	`description` text NOT NULL,
	`created_at` text NOT NULL,
	`updated_at` text NOT NULL
);
--> statement-breakpoint
CREATE UNIQUE INDEX `auth_roles_name_unique` ON `auth_roles` (`name`);--> statement-breakpoint
CREATE TABLE `auth_settings` (
	`id` text PRIMARY KEY NOT NULL,
	`default_role_id` text,
	`updated_at` text NOT NULL,
	FOREIGN KEY (`default_role_id`) REFERENCES `auth_roles`(`id`) ON UPDATE no action ON DELETE set null
);
--> statement-breakpoint
CREATE TABLE `auth_subject_role_bindings` (
	`id` text PRIMARY KEY NOT NULL,
	`role_id` text NOT NULL,
	`issuer` text NOT NULL,
	`subject` text NOT NULL,
	FOREIGN KEY (`role_id`) REFERENCES `auth_roles`(`id`) ON UPDATE no action ON DELETE cascade
);
--> statement-breakpoint
CREATE UNIQUE INDEX `auth_subject_role_bindings_unique` ON `auth_subject_role_bindings` (`role_id`,`issuer`,`subject`);--> statement-breakpoint
CREATE TABLE `auth_user_roles` (
	`user_id` text NOT NULL,
	`role_id` text NOT NULL,
	PRIMARY KEY(`user_id`, `role_id`),
	FOREIGN KEY (`user_id`) REFERENCES `auth_users`(`id`) ON UPDATE no action ON DELETE cascade,
	FOREIGN KEY (`role_id`) REFERENCES `auth_roles`(`id`) ON UPDATE no action ON DELETE cascade
);
--> statement-breakpoint
CREATE TABLE `auth_users` (
	`id` text PRIMARY KEY NOT NULL,
	`status` text NOT NULL,
	`created_at` text NOT NULL,
	`updated_at` text NOT NULL
);
