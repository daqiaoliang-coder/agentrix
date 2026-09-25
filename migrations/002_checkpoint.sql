-- agentrix HITL 检查点持久化建表脚本
--
-- 承载 eino 审批中断时序列化的图状态，使 Resume 可跨进程重启、多副本生效。
-- 检查点只按 checkpoint_id 整存整取，框架不解析内容，故用 BLOB 原样存储。
--
-- 执行：mysql -u root -p agentrix < 002_checkpoint.sql

CREATE TABLE IF NOT EXISTS `ai_checkpoint` (
  `checkpoint_id` VARCHAR(128) NOT NULL                COMMENT '检查点 ID（= session_id）',
  `checkpoint`    LONGBLOB     NOT NULL                COMMENT 'eino 序列化后的图状态（不透明字节）',
  `create_time`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '首次写入时间',
  `update_time`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '最后覆盖时间',
  PRIMARY KEY (`checkpoint_id`),
  KEY `idx_update_time` (`update_time`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci
  COMMENT = 'HITL 检查点：审批中断的图状态现场，支持跨进程恢复';
