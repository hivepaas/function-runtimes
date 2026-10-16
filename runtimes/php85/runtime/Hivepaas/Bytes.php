<?php

declare(strict_types=1);

namespace Hivepaas;

/**
 * A response body sent as the bytes it is, application/octet-stream: a body
 * that is a string is sent as text.
 */
final readonly class Bytes
{
    public function __construct(public string $data)
    {
    }
}
