<?php

declare(strict_types=1);

namespace Hivepaas;

/** What a call is besides its request: its id, its deadline, its log. */
final readonly class Context
{
    /**
     * @param string $requestId the call's id
     * @param float  $deadline  when the call times out, in seconds since the epoch, as microtime(true)
     */
    public function __construct(
        public string $requestId,
        public float $deadline,
    ) {
    }

    /** Writes a line to the call's log, which carries the call's id: the values, joined by spaces. */
    public function log(mixed ...$values): void
    {
        $line = json_encode(
            ['hp' => 'log', 'requestId' => $this->requestId, 'msg' => implode(' ', array_map(self::text(...), $values))],
            JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_INVALID_UTF8_SUBSTITUTE,
        );
        fwrite(STDOUT, $line . "\n");
    }

    private static function text(mixed $value): string
    {
        return match (true) {
            is_string($value) => $value,
            $value === null => 'null',
            is_bool($value) => $value ? 'true' : 'false',
            is_int($value), is_float($value), $value instanceof \Stringable => (string) $value,
            default => json_encode($value, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_PARTIAL_OUTPUT_ON_ERROR)
                ?: get_debug_type($value),
        };
    }
}
