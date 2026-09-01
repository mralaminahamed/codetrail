-- 0004 added repos.status and nothing has ever written or read it. A column
-- whose only value is its default is a claim the schema makes and the code
-- does not keep; P3 can add one when it knows what the states are.
ALTER TABLE repos DROP COLUMN IF EXISTS status;
