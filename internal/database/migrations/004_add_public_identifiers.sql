UPDATE deployments
SET public_identifier = replace(id::text, '-', '')
WHERE public_identifier IS NULL;

ALTER TABLE deployments
    ALTER COLUMN public_identifier SET NOT NULL;
