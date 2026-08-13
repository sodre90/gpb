-- Runs cut off by a restart were recorded as failures, which is what the daemon reads when it
-- asks whether a backup is owed: a whole day would pass with the schedule believing the day's
-- run had been attempted and answered. The error text says plainly what happened to them, so
-- they can be told apart from runs that failed for their own reasons.
UPDATE sync_runs SET outcome = 'interrupted'
WHERE outcome = 'error' AND error = 'context canceled';
