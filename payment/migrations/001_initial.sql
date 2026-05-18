-- payment-service initial schema (MySQL 8).
-- Equivalent to what GORM AutoMigrate produces; shipped for manual review and
-- production change-management workflows.
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
  `id`                     BIGINT          NOT NULL AUTO_INCREMENT,
  `order_no`               VARCHAR(32)     NOT NULL,
  `user_id`                INT             NOT NULL,
  `amount_idr`             BIGINT          NOT NULL,
  `fee_idr`                BIGINT          DEFAULT 0,
  `net_idr`                BIGINT          DEFAULT 0,
  `exchange_rate`          DECIMAL(20,8)   NOT NULL,
  `quota_to_credit`        BIGINT          NOT NULL,
  `quota_credited`         BIGINT          DEFAULT 0,
  `payment_method`         VARCHAR(32)     NOT NULL,
  `xendit_invoice_id`      VARCHAR(64)     DEFAULT '',
  `xendit_payment_id`      VARCHAR(64)     DEFAULT '',
  `xendit_payment_channel` VARCHAR(32)     DEFAULT '',
  `checkout_url`           TEXT,
  `qr_string`              TEXT,
  `va_number`              VARCHAR(32)     DEFAULT '',
  `status`                 VARCHAR(24)     NOT NULL,
  `failure_reason`         VARCHAR(128)    DEFAULT '',
  `created_at`             DATETIME(3)     NOT NULL,
  `expires_at`             DATETIME(3)     NOT NULL,
  `paid_at`                DATETIME(3)     DEFAULT NULL,
  `credited_at`            DATETIME(3)     DEFAULT NULL,
  `client_ip`              VARCHAR(45)     DEFAULT '',
  `user_agent`             VARCHAR(255)    DEFAULT '',
  `metadata_json`          TEXT,
  `updated_at`             DATETIME(3)     NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_orders_order_no` (`order_no`),
  KEY `idx_orders_user_created` (`user_id`, `created_at` DESC),
  KEY `idx_orders_status_expires` (`status`, `expires_at`),
  KEY `idx_orders_xendit_invoice` (`xendit_invoice_id`),
  KEY `idx_orders_xendit_payment` (`xendit_payment_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------
-- webhook_events  (correction 5: composite UNIQUE for retransmission dedupe)
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `webhook_events` (
  `id`                  BIGINT       NOT NULL AUTO_INCREMENT,
  `event_type`          VARCHAR(64)  NOT NULL,
  `xendit_resource_id`  VARCHAR(64)  NOT NULL,
  `order_no`            VARCHAR(32)  DEFAULT '',
  `raw_payload`         MEDIUMTEXT,
  `signature`           VARCHAR(256) DEFAULT '',
  `source_ip`           VARCHAR(45)  DEFAULT '',
  `received_at`         DATETIME(3)  NOT NULL,
  `processed_at`        DATETIME(3)  DEFAULT NULL,
  `process_result`      VARCHAR(24)  DEFAULT '',
  `error_msg`           VARCHAR(512) DEFAULT '',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_webhook_event_resource` (`event_type`, `xendit_resource_id`),
  KEY `idx_webhook_events_order_no` (`order_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------
-- topup_callbacks  (correction 6: composite UNIQUE)
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
-- refunds  (table only in PR #2 - code lands in PR #3)
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `refunds` (
  `id`               BIGINT      NOT NULL AUTO_INCREMENT,
  `refund_no`        VARCHAR(32) DEFAULT '',
  `order_no`         VARCHAR(32) NOT NULL,
  `refund_type`      VARCHAR(24) NOT NULL,
  `amount_idr`       BIGINT      NOT NULL,
  `quota_to_deduct`  BIGINT      NOT NULL,
  `reason`           VARCHAR(255) DEFAULT '',
  `operator_id`      INT         NOT NULL,
  `status`           VARCHAR(24) NOT NULL,
  `xendit_refund_id` VARCHAR(64) DEFAULT '',
  `created_at`       DATETIME(3) NOT NULL,
  `completed_at`     DATETIME(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_refunds_refund_no` (`refund_no`),
  KEY `idx_refunds_order_no` (`order_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------
-- payment_config (KV store; seeds are inserted by the application on startup)
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `payment_config` (
  `config_key`   VARCHAR(64)  NOT NULL,
  `config_value` TEXT,
  `description`  VARCHAR(255) DEFAULT '',
  `updated_at`   DATETIME(3),
  `updated_by`   INT          DEFAULT 0,
  PRIMARY KEY (`config_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
