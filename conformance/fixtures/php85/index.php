<?php

declare(strict_types=1);

// The function the conformance suite calls: each path shows one thing the
// contract promises.

use FixtureLib\Lib;
use Hivepaas\Bytes;
use Hivepaas\Context;
use Hivepaas\Request;

function handler(Request $req, Context $ctx): ?array
{
    $q = $req->query;
    return match ($req->path) {
        '/' => null,
        '/json' => ['body' => ['hello' => $q['name'] ?? null]],
        '/text' => ['body' => 'hello'],
        '/bytes' => ['body' => new Bytes("\x00\x01\x02\xff")],
        '/echo' => ['body' => [
            'method' => $req->method,
            'path' => $req->path,
            'query' => $q['a'] ?? null,
            'queryAll' => $req->queryAll['a'] ?? null,
            'header' => $req->headers['x-custom'] ?? null,
            'body' => $req->text(),
            'requestId' => $ctx->requestId,
        ]],
        '/status' => ['status' => (int) $q['code']],
        '/error' => throw new RuntimeException('fixture failure'),
        '/slow' => slow((int) $q['ms']), // past the timeout on purpose
        '/big' => ['body' => str_repeat('x', (int) $q['bytes'])],
        '/invalid' => ['status' => 42],
        '/env' => ['body' => (string) getenv('GREETING')],
        '/deadline' => ['body' => ['remainingMs' => (int) (($ctx->deadline - microtime(true)) * 1000)]],
        '/log' => logged($ctx),
        '/pid' => ['body' => ['pid' => getmypid()]],
        '/headers' => ['body' => ['names' => sorted(array_keys($req->headers))]],
        '/whoami' => ['body' => ['uid' => posix_getuid(), 'gid' => posix_getgid()]],
        '/lib' => ['body' => Lib::name()],
        default => ['status' => 404, 'body' => ['error' => 'no route']],
    };
}

function slow(int $ms): array
{
    usleep($ms * 1000);
    return ['body' => 'slow done'];
}

function logged(Context $ctx): array
{
    $ctx->log('fixture log line');
    return ['body' => 'logged'];
}

/** @param list<string> $names */
function sorted(array $names): array
{
    sort($names);
    return $names;
}
