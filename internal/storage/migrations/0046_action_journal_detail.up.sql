-- What each journalled write actually set: the overwrite's target and bits,
-- the roles a member edit left them with, the role added or removed. The
-- journal said a permission call was made on a channel; it could not say
-- for whom or to what, which is the question when an overwrite turns up
-- that nobody remembers writing.
ALTER TABLE action_journal ADD COLUMN detail TEXT NOT NULL DEFAULT '';
