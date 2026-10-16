<?php

declare(strict_types=1);

use Hivepaas\Bytes;
use Hivepaas\Runtime\Response;

// What a handler returns, as it is sent.
function testNothingIsNoContent(): void
{
    same([['status' => 204, 'headers' => []], ''], Response::of(null, 100));
}

function testABodyIsSentWithTheTypeItHas(): void
{
    same([['status' => 200, 'headers' => ['content-type' => ['text/plain; charset=utf-8']]], 'hello'],
        Response::of(['body' => 'hello'], 100), 'text');
    same([['status' => 200, 'headers' => ['content-type' => ['application/octet-stream']]], "\x00\xff"],
        Response::of(['body' => new Bytes("\x00\xff")], 100), 'bytes');
    same([['status' => 201, 'headers' => ['content-type' => ['application/json']]], '{"a/b":"é","n":[1,2]}'],
        Response::of(['status' => 201, 'body' => ['a/b' => 'é', 'n' => [1, 2]]], 100), 'JSON');
    same([['status' => 200, 'headers' => []], ''], Response::of([], 100), 'an empty response');
}

function testTheHandlersHeadersAreKept(): void
{
    [$head, $body] = Response::of(
        ['headers' => ['Content-Type' => 'text/csv', 'Set-Cookie' => ['a=1', 'b=2'], 'X-Count' => 3], 'body' => 'x'],
        100,
    );
    same(['content-type' => ['text/csv'], 'set-cookie' => ['a=1', 'b=2'], 'x-count' => ['3']], $head['headers']);
}

function testAStatusThatCarriesNoBodySendsNone(): void
{
    same([['status' => 304, 'headers' => []], ''], Response::of(['status' => 304, 'body' => 'x'], 100));
}

function testWhatIsNotAResponseFailsTheCall(): void
{
    fails(fn () => Response::of('hello', 100), 'not a response');
    fails(fn () => Response::of([1, 2], 100), 'not a response');
    fails(fn () => Response::of(['status' => 42], 100), 'status 42 is not a whole number from 200 to 599');
    fails(fn () => Response::of(['status' => '200'], 100), "status '200'");
    fails(fn () => Response::of(['headers' => 'x'], 100), 'headers are not');
    fails(fn () => Response::of(['headers' => ['X-A' => "1\r\nX-B: 2"]], 100), 'line break');
    fails(fn () => Response::of(['headers' => ['X A' => '1']], 100), "not a header's name");
    fails(fn () => Response::of(['body' => NAN], 100), 'cannot be JSON');
    fails(fn () => Response::of(['body' => str_repeat('x', 101)], 100), '101 bytes', 'response_too_large');
}
