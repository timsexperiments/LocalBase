ALTER TABLE `api_keys` ADD `scopes` text DEFAULT '[]' NOT NULL;
--> statement-breakpoint
UPDATE `api_keys` SET `scopes` = '["inference:chat","inference:embeddings","inference:image","inference:video","inference:speech","inference:transcription","models:read"]';
