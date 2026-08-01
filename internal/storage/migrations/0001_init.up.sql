-- Placeholder for Milestone 1. Domain tables (audit, rotation, factions)
-- are added by the milestones that need them.
CREATE TABLE IF NOT EXISTS schema_migrations (
    version BIGINT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
