-- Loaded by mysql docker-entrypoint on FIRST boot only.
-- Creates the separate payment schema and grants access to the same
-- `oneapi` user one-api already uses.

CREATE DATABASE IF NOT EXISTS `oneapi_payment` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
GRANT ALL PRIVILEGES ON `oneapi_payment`.* TO 'oneapi'@'%';
FLUSH PRIVILEGES;
