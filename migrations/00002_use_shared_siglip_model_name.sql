-- +goose Up
UPDATE embeddings
SET model = 'google/siglip2-base-patch16-256'
WHERE model = 'siglip2-vision';

-- +goose Down
UPDATE embeddings
SET model = 'siglip2-vision'
WHERE model = 'google/siglip2-base-patch16-256';
