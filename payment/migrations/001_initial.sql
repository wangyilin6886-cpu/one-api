-- payment-service initial schema (MySQL 8).
-- Equivalent to what GORM AutoMigrate produces; shipped for manual review
-- and production change-management workflows.
--
-- Apply against a fresh schema:
--   CREATE DATABASE oneapi_payment CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
--   USE oneapi_payment;
--   SOURCE 001_initial.sql;

SET NAMES utf8mb4;

-- ---------------------------------------------------------------------
-- orders
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `orders` (
  `id`                    BIGINT       NOT NULL AUTO_INCREMENT,
  `order_no`              VARCHAR(32)  NOT NULL,
  `user_id`               INT          NOT NULL,
  `order_type`            VARCHAR(24)  NOT NULL DEFAULT 'topup',
  `subscription_id`       VARCHAR(64)  DEFAULT '',
  `amount_usd_cents`      BIGINT       NOT NULL,
  `currency`              VARCHAR(8)   NOT NULL DEFAULT 'USD',
  `quota_to_credit`       BIGINT       NOT NULL,
  `quota_credited`        BIGINT       DEFAULT 0,
  `provider`              VARCHAR(32)  NOT NULL,
  `provider_checkout_id`  VARCHAR(128) DEFAULT '',
  `provider_payment_id`   VARCHAR(128) DEFAULT '',
  `checkout_url`          TEXT,
  `status`                VARCHAR(24)  NOT NULL,
  `failure_reason`        VARCHAR(255) DEFAULT '',
  `created_at`            DATETIME(3)  NOT NULL,
  `expires_at`            DATETIME(3)  NOT NULL,
  `paid_at`               DATETIME(3)  DEFAULT NULL,
  `credited_at`           DATETIME(3)  DEFAULT NULL,
  `client_ip`             VARCHAR(45)  DEFAULT '',
  `user_agent`            VARCHAR(255) DEFAULT '',
  `metadata_json`         TEXT,
  `updated_at`            DATETIME(3)  NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_orders_order_no` (`order_no`),
  KEY `idx_orders_user_created` (`user_id`, `created_at` DESC),
  KEY `idx_orders_status_expires` (`status`, `expires_at`),
  KEY `idx_orders_subscription` (`subscription_id`),
  KEY `idx_orders_provider_checkout` (`provider_checkout_id`),
  KEY `idx_orders_provider_payment` (`provider_payment_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------
-- webhook_events
-- UNIQUE(event_type, provider_resource_id) is the dedupe layer against
-- provider retransmissions.
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `webhook_events` (
  `id`                    BIGINT       NOT NULL AUTO_INCREMENT,
  `provider`              VARCHAR(32)  NOT NULL DEFAULT 'polar',
  `event_type`            VARCHAR(64)  NOT NULL,
  `provider_resource_id`  VARCHAR(128) NOT NULL,
  `order_no`              VARCHAR(32)  DEFAULT '',
  `raw_payload`           MEDIUMTEXT,
  `signature`             VARCHAR(512) DEFAULT '',
  `source_ip`             VARCHAR(45)  DEFAULT '',
  `received_at`           DATETIME(3)  NOT NULL,
  `processed_at`          DATETIME(3)  DEFAULT NULL,
  `process_result`        VARCHAR(24)  DEFAULT '',
  `error_msg`             VARCHAR(512) DEFAULT '',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_webhook_event_resource` (`event_type`, `provider_resource_id`),
  KEY `idx_webhook_events_order_no` (`order_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------
-- topup_callbacks
-- UNIQUE(order_no, action_type) dedupes outgoing /api/internal/topup calls.
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `topup_callbacks` (
  `id`            BIGINT      NOT NULL AUTO_INCREMENT,
  `order_no`      VARCHAR(32) NOT NULL,
  `action_type`   VARCHAR(24) NOT NULL,
  `user_id`       INT         NOT NULL,
  `quota`         BIGINT      NOT NULL,
  `request_body`  TEXT,
  `response_body` TEXT,
  `http_status`   INT         DEFAULT 0,
  `attempt`       INT         DEFAULT 1,
  `succeeded_at`  DATETIME(3) DEFAULT NULL,
  `created_at`    DATETIME(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_topup_cb_order_action` (`order_no`, `action_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------
-- refunds (table only; code lands in PR-D)
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `refunds` (
  `id`                  BIGINT       NOT NULL AUTO_INCREMENT,
  `refund_no`           VARCHAR(32)  DEFAULT '',
  `order_no`            VARCHAR(32)  NOT NULL,
  `refund_type`         VARCHAR(24)  NOT NULL,
  `amount_usd_cents`    BIGINT       NOT NULL,
  `currency`            VARCHAR(8)   NOT NULL DEFAULT 'USD',
  `quota_to_deduct`     BIGINT       NOT NULL,
  `reason`              VARCHAR(255) DEFAULT '',
  `operator_id`         INT          NOT NULL,
  `status`              VARCHAR(24)  NOT NULL,
  `provider_refund_id`  VARCHAR(128) DEFAULT '',
  `created_at`          DATETIME(3)  NOT NULL,
  `completed_at`        DATETIME(3)  DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_refunds_refund_no` (`refund_no`),
  KEY `idx_refunds_order_no` (`order_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------
-- payment_config (KV; seeds inserted by the application on startup)
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `payment_config` (
  `config_key`   VARCHAR(64)  NOT NULL,
  `config_value` TEXT,
  `description`  VARCHAR(255) DEFAULT '',
  `updated_at`   DATETIME(3),
  `updated_by`   INT          DEFAULT 0,
  PRIMARY KEY (`config_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
