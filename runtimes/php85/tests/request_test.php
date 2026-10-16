<?php

declare(strict_types=1);

use Hivepaas\Context;
use Hivepaas\Request;

function testTheRequestsBody(): void
{
    $req = new Request('POST', '/', [], [], [], '{"name":"Ada","tags":["a"]}');
    same('{"name":"Ada","tags":["a"]}', $req->text());
    same(['name' => 'Ada', 'tags' => ['a']], $req->json());
    try {
        (new Request('POST', '/', [], [], [], 'not json'))->json();
        throw new \RuntimeException('a body that is not JSON was decoded');
    } catch (\JsonException) {
    }
}

// A log line carries the call's id, the values joined as text.
function testALogLineCarriesTheCallsID(): void
{
    $script = sprintf(
        'require %s; require %s; (new Hivepaas\Context("req-1", 0.0))->log("count", 3, true, null, ["a" => 1], 1.5);',
        var_export(__DIR__ . '/../runtime/Hivepaas/Request.php', true),
        var_export(__DIR__ . '/../runtime/Hivepaas/Context.php', true),
    );
    $out = shell_exec(escapeshellarg(PHP_BINARY) . ' -r ' . escapeshellarg($script));
    same('{"hp":"log","requestId":"req-1","msg":"count 3 true null {\"a\":1} 1.5"}' . "\n", $out);
}
