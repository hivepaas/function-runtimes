<?php

declare(strict_types=1);

namespace Hivepaas;

/** A call's request. */
final readonly class Request
{
    /**
     * @param string                      $method   in upper case
     * @param string                      $path     without the query, as sent (still percent-encoded)
     * @param array<string, string>       $query    the first value of each name
     * @param array<string, list<string>> $queryAll every value of each name
     * @param array<string, string>       $headers  by name in lower case; a header sent several times
     *                                              is one value, its values joined by ", "
     * @param string                      $body     the body's bytes
     */
    public function __construct(
        public string $method,
        public string $path,
        public array $query,
        public array $queryAll,
        public array $headers,
        public string $body,
    ) {
    }

    /** The body as text. */
    public function text(): string
    {
        return $this->body;
    }

    /**
     * The body as JSON, its objects as arrays.
     *
     * @throws \JsonException when it is not JSON
     */
    public function json(): mixed
    {
        return json_decode($this->body, true, 512, JSON_THROW_ON_ERROR);
    }
}
