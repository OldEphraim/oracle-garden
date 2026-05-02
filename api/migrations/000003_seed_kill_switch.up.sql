-- 000003_seed_kill_switch.up.sql
-- Initial DB-side state for the global execution kill switch. The boot value
-- is gated by EXECUTION_KILL_SWITCH_DEFAULT in the API process; this row is
-- the persisted value the admin endpoint flips at runtime.
INSERT INTO system_config (key, value)
VALUES ('kill_switch', 'false'::jsonb);
