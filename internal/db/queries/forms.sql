
-- name: DeleteFormSubmissionsBySiteOlderThan :execrows
-- Per-site retention sweep. form_submissions.data_json is the raw submitted
-- payload (name, email, phone, free text) and the only delete that existed was
-- single-row by id, so a contact form accumulated PII forever while the
-- tenant's analytics window suggested otherwise.
DELETE FROM form_submissions
WHERE site_id = ? AND created_at < ?;
