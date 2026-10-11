-- Extra roles the join-role script gives every new member, on top of the
-- member role (0040). A role somebody needs to function in the guild that is
-- not "the" member role, such as a media-permissions role.
ALTER TABLE settings_guild ADD COLUMN join_role_ids TEXT[] NOT NULL DEFAULT '{}';
