-- agentrix 会话执行租约建表脚本
--
-- 保证同一会话同一时刻只有一个 Turn（Run/Resume）执行：并发 Resume 同一审批
-- 会重复执行写工具，多副本部署下必须由共享存储仲裁。
-- 租约带过期时间，持有进程崩溃后自动失效，其他副本可接管。
--
-- 执行：mysql -u root -p agentrix < 003_session_lease.sql

CREATE TABLE IF NOT EXISTS `ai_session_lease` (
  `session_id`  VARCHAR(128) NOT NULL COMMENT '会话 ID（含子代理会话 parent::key[::suffix]）',
  `owner`       VARCHAR(64)  NOT NULL COMMENT '持有方标识：每次执行随机生成',
  `expire_time` DATETIME(3)  NOT NULL COMMENT '过期时间：以数据库 NOW(3) 为准',
  PRIMARY KEY (`session_id`),
  KEY `idx_expire_time` (`expire_time`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci
  COMMENT = '会话执行租约：Run/Resume 互斥';
