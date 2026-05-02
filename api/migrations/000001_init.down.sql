-- 000001_init.down.sql
-- Reverse 000001_init.up.sql. Drops in FK-safe order; CASCADE on each is
-- belt-and-suspenders against any objects added by later migrations that
-- haven't been reverted yet.

DROP TABLE IF EXISTS strategy_market_subscriptions CASCADE;
DROP TABLE IF EXISTS system_config                 CASCADE;
DROP TABLE IF EXISTS user_usage_daily              CASCADE;
DROP TABLE IF EXISTS paper_trades                  CASCADE;
DROP TABLE IF EXISTS agent_steps                   CASCADE;
DROP TABLE IF EXISTS workflow_runs                 CASCADE;
DROP TABLE IF EXISTS workflow_edges                CASCADE;
DROP TABLE IF EXISTS workflow_nodes                CASCADE;
DROP TABLE IF EXISTS workflows                     CASCADE;
DROP TABLE IF EXISTS agent_templates               CASCADE;
DROP TABLE IF EXISTS type_definitions              CASCADE;
DROP TABLE IF EXISTS users                         CASCADE;

-- pgcrypto extension intentionally left in place — it's infrastructure and
-- may be used by other databases on the same host.
