<?php

declare(strict_types=1);

namespace Hivepaas\Runtime;

use Hivepaas\Bytes;

/** What a handler returned, as the server sends it. */
final class Response
{
    private const int JSON_FLAGS = JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE;

    /**
     * The reply's head - status and headers - and the body, of what the handler returned: null, or an array
     * with any of status, headers and body.
     *
     * @return array{0: array{status: int, headers: array<string, list<string>>}, 1: string}
     *
     * @throws CallError when it is not a response
     */
    public static function of(mixed $value, int $maxBodySize): array
    {
        if ($value === null) {
            return [['status' => 204, 'headers' => []], ''];
        }
        if (!is_array($value) || ($value !== [] && array_is_list($value))) {
            throw new CallError(
                'the handler returned what is not a response: an array with status, headers and body',
            );
        }
        $status = $value['status'] ?? 200;
        if (!is_int($status) || $status < 200 || $status > 599) {
            throw new CallError(sprintf(
                "the response's status %s is not a whole number from 200 to 599",
                var_export($status, true),
            ));
        }
        $headers = self::headers($value['headers'] ?? []);
        [$body, $type] = self::body($value['body'] ?? null);
        if (strlen($body) > $maxBodySize) {
            throw new CallError(sprintf(
                "the response's body of %d bytes is larger than HP_FN_MAX_BODY_SIZE, %d",
                strlen($body),
                $maxBodySize,
            ), 'response_too_large');
        }
        if ($status === 204 || $status === 304) {
            return [['status' => $status, 'headers' => $headers], ''];
        }
        if ($body !== '' && $type !== null && !isset($headers['content-type'])) {
            $headers['content-type'] = [$type];
        }
        return [['status' => $status, 'headers' => $headers], $body];
    }

    /**
     * Headers by name in lower case, each with its values.
     *
     * @return array<string, list<string>>
     */
    private static function headers(mixed $given): array
    {
        if (!is_array($given)) {
            throw new CallError("the response's headers are not an array of names and values");
        }
        $headers = [];
        foreach ($given as $name => $value) {
            $name = (string) $name;
            if (preg_match('/^[!#$%&\'*+\-.^_`|~0-9A-Za-z]+$/', $name) !== 1) {
                throw new CallError(sprintf("the response's header %s is not a header's name", json_encode($name)));
            }
            $values = [];
            foreach (is_array($value) ? $value : [$value] as $one) {
                $one = match (true) {
                    is_string($one) => $one,
                    is_bool($one) => $one ? 'true' : 'false',
                    is_int($one), is_float($one), $one instanceof \Stringable => (string) $one,
                    default => throw new CallError(sprintf(
                        "the response's header %s has a value that is not text: %s",
                        $name,
                        get_debug_type($one),
                    )),
                };
                if (preg_match('/[\r\n\0]/', $one) === 1) {
                    throw new CallError(sprintf("the response's header %s has a line break in its value", $name));
                }
                $values[] = $one;
            }
            $key = strtolower($name);
            $headers[$key] = array_merge($headers[$key] ?? [], $values);
        }
        return $headers;
    }

    /**
     * The body's bytes, and the type it is sent with when the headers name none.
     *
     * @return array{0: string, 1: ?string}
     */
    private static function body(mixed $body): array
    {
        return match (true) {
            $body === null => ['', null],
            is_string($body) => [$body, 'text/plain; charset=utf-8'],
            $body instanceof Bytes => [$body->data, 'application/octet-stream'],
            default => [self::json($body), 'application/json'],
        };
    }

    private static function json(mixed $body): string
    {
        try {
            return json_encode($body, self::JSON_FLAGS);
        } catch (\JsonException $e) {
            throw new CallError("the response's body cannot be JSON: " . $e->getMessage());
        }
    }
}
