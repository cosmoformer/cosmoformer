SET search_path = cosmoformer, pg_catalog;

-- Reserve bounded accounting before password verification. A missing subject
-- must never be admitted when its failures cannot be retained. Serialize the
-- reservation with failure accounting, expiry and successful-login release.
CREATE OR REPLACE FUNCTION auth_rate(subject_hash bytea, outcome text) RETURNS integer LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
DECLARE r auth_throttles; n timestamptz:=clock_timestamp();
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 DELETE FROM auth_throttles WHERE updated_at<n-interval '30 minutes';
 SELECT * INTO r FROM auth_throttles WHERE subject=subject_hash;
 IF r.blocked_until>n THEN RETURN ceil(extract(epoch FROM r.blocked_until-n))::integer; END IF;
 IF outcome='success' THEN DELETE FROM auth_throttles WHERE subject=subject_hash; RETURN 0; END IF;
 IF outcome NOT IN ('check','failure') OR outcome IS NULL THEN RAISE EXCEPTION 'invalid throttle outcome'; END IF;
 IF r.subject IS NULL THEN
  IF (SELECT count(*) FROM auth_throttles)>=10000 THEN RETURN 60; END IF;
  INSERT INTO auth_throttles(subject,failures,window_at,updated_at) VALUES(subject_hash,0,n,n);
 END IF;
 IF outcome='check' THEN RETURN 0; END IF;
 UPDATE auth_throttles SET failures=CASE WHEN failures=0 OR window_at<n-interval '15 minutes' THEN 1 ELSE failures+1 END,
  window_at=CASE WHEN failures=0 OR window_at<n-interval '15 minutes' THEN n ELSE window_at END,updated_at=n,
  blocked_until=CASE WHEN window_at>=n-interval '15 minutes' AND failures+1>=5 THEN n+interval '15 minutes' ELSE NULL END
  WHERE subject=subject_hash;
 RETURN 0;
END $$;
