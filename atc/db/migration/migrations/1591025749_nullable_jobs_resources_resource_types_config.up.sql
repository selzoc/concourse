
  ALTER TABLE jobs ALTER COLUMN config DROP NOT NULL;
  ALTER TABLE resources ALTER COLUMN config DROP NOT NULL;
  ALTER TABLE resource_types ALTER COLUMN config DROP NOT NULL;