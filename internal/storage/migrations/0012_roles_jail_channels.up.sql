-- Which channels stay visible to a jailed member is guild configuration
-- (like mod_role_ids), not jail/grant runtime state, so it lives here
-- rather than in the roles plugin's own store (role_jails/role_grants).
-- Denial is enforced via permission overwrites on the shared "Jailed" role
-- (internal/plugins/roles), not per-member overwrites: every channel not in
-- this allowlist gets a deny overwrite for that one role, so jailing a
-- member never needs any per-channel API calls at all — only this
-- allowlist (rarely changed) does.
ALTER TABLE settings_guild ADD COLUMN jail_allowed_channel_ids TEXT[] NOT NULL DEFAULT '{}';
