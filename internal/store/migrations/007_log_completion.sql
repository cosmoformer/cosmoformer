SET search_path = cosmoformer, pg_catalog;
-- Archive rows describe segments/gaps, but cannot distinguish an open stream
-- from a worker's final receipt. Preserve that bounded receipt on its attempt.
ALTER TABLE attempts ADD COLUMN log_final jsonb
 CHECK(log_final IS NULL OR (jsonb_typeof(log_final)='object' AND octet_length(log_final::text)<=1024));
