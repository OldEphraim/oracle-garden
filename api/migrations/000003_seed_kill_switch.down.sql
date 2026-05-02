-- 000003_seed_kill_switch.down.sql
DELETE FROM system_config WHERE key = 'kill_switch';
