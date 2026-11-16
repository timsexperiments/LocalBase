CREATE TABLE `auth_user_emails` (
	`user_id` text PRIMARY KEY NOT NULL,
	`email` text NOT NULL,
	FOREIGN KEY (`user_id`) REFERENCES `auth_users`(`id`) ON UPDATE no action ON DELETE cascade
);
--> statement-breakpoint
CREATE UNIQUE INDEX `auth_user_emails_email_unique` ON `auth_user_emails` (`email`);
--> statement-breakpoint
CREATE UNIQUE INDEX `auth_identities_user_id_unique` ON `auth_identities` (`user_id`);
