ALTER TABLE `config` ADD `selected_video_models` text NOT NULL DEFAULT '[]';
--> statement-breakpoint
ALTER TABLE `config` ADD `active_video_model` text NOT NULL DEFAULT '';
