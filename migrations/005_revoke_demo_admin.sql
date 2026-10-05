UPDATE users
SET
    role = 'user',
    updated_at = now()
WHERE lower(email) = 'admin@seat-reservation.demo';
