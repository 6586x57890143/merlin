-- The ordinary member role (roles plugin, jailchannels.go memberBaseline):
-- what a plain member of this guild holds besides @everyone, the Melting
-- Pot's "melted". A marker role (jailed, on vacation) is never granted a
-- permission on an allowlisted channel that this role cannot use there.
-- NULL means the baseline is @everyone alone.
ALTER TABLE settings_guild ADD COLUMN member_role_id TEXT;
