-- One reminder per paid period: the roles-supporters job claims the row
-- before it sends the DM, so a period ending at the same instant is never
-- reminded twice, whichever process or guild got there first.
CREATE TABLE supporter_reminders (
    user_id TEXT        NOT NULL,
    until   TIMESTAMPTZ NOT NULL,
    sent_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, until)
);
