CREATE TABLE `model_runtime_capabilities` (
	`model_id` text NOT NULL,
	`artifact_sha256` text NOT NULL,
	`capabilities` text NOT NULL,
	`input_modalities` text NOT NULL,
	`build_info` text NOT NULL,
	`observed_at` text NOT NULL,
	PRIMARY KEY(`model_id`, `artifact_sha256`)
);
