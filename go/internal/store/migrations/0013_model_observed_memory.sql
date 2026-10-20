CREATE TABLE `model_observed_memory` (
	`model_id` text NOT NULL,
	`artifact_sha256` text NOT NULL,
	`launch_shape` text NOT NULL,
	`peak_bytes` integer NOT NULL,
	`method` text NOT NULL,
	`observed_at` text NOT NULL,
	PRIMARY KEY(`model_id`, `artifact_sha256`, `launch_shape`)
);
