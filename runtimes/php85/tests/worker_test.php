<?php

declare(strict_types=1);

// The worker speaks the server's protocol: its first reply says the handler
// is loaded, then each call is answered after a sync line on its output.
function testTheWorkerAnswersCalls(): void
{
    $app = sys_get_temp_dir() . '/hivepaas-worker-test-' . getmypid();
    @mkdir($app);
    file_put_contents($app . '/index.php', <<<'PHP'
        <?php
        $greeting = 'hello';
        function handler(Hivepaas\Request $req, Hivepaas\Context $ctx): array {
            global $greeting;
            echo "printed\n";
            if ($req->path === '/fail') {
                throw new RuntimeException('boom');
            }
            return ['body' => ['greeting' => $greeting, 'path' => $req->path, 'body' => $req->body,
                'header' => $req->headers['x-a'] ?? null, 'id' => $ctx->requestId]];
        }
        PHP);
    $proc = proc_open(
        [PHP_BINARY, __DIR__ . '/../runtime/worker.php'],
        [1 => ['pipe', 'w'], 2 => ['pipe', 'w'], 3 => ['pipe', 'r'], 4 => ['pipe', 'w']],
        $pipes,
        $app,
        ['HP_FN_ENTRYPOINT' => 'index.php', 'HP_FN_HANDLER' => 'handler', 'HP_FN_MAX_BODY_SIZE' => '1000'],
    );
    try {
        same('{"loaded":true,"bodyLength":0}' . "\n", fgets($pipes[4]), 'the load');

        $call = ['seq' => 1, 'id' => 'r1', 'method' => 'POST', 'path' => '/x', 'query' => (object) [],
            'headers' => ['x-a' => ['1', '2']], 'deadline' => 0, 'bodyLength' => 4];
        fwrite($pipes[3], json_encode($call) . "\nbody");
        $head = json_decode(fgets($pipes[4]), true);
        $body = fread($pipes[4], $head['bodyLength']);
        same(200, $head['status']);
        same(['greeting' => 'hello', 'path' => '/x', 'body' => 'body', 'header' => '1, 2', 'id' => 'r1'],
            json_decode($body, true));
        same("printed\n\x1fhivepaas-sync:1\n", fread($pipes[1], 100), 'the output and its sync');

        $call = ['seq' => 2, 'path' => '/fail', 'bodyLength' => 0] + $call;
        fwrite($pipes[3], json_encode($call) . "\n");
        $head = json_decode(fgets($pipes[4]), true);
        same(2, $head['seq']);
        same(true, str_starts_with($head['error'], 'RuntimeException: boom'), 'the error');
    } finally {
        fclose($pipes[3]);
        same(0, proc_close($proc), 'the exit once the calls end');
        unlink($app . '/index.php');
        rmdir($app);
    }
}

function testAWorkerWithoutTheHandlerSaysSo(): void
{
    $app = sys_get_temp_dir() . '/hivepaas-worker-test-missing-' . getmypid();
    @mkdir($app);
    file_put_contents($app . '/index.php', "<?php\n");
    $proc = proc_open(
        [PHP_BINARY, __DIR__ . '/../runtime/worker.php'],
        [1 => ['pipe', 'w'], 2 => ['pipe', 'w'], 3 => ['pipe', 'r'], 4 => ['pipe', 'w']],
        $pipes,
        $app,
        ['HP_FN_ENTRYPOINT' => 'index.php', 'HP_FN_HANDLER' => 'missing'],
    );
    $reply = json_decode(fgets($pipes[4]), true);
    same(false, $reply['loaded']);
    same(true, str_ends_with($reply['error'], '/index.php has no function missing'), $reply['error']);
    fclose($pipes[3]);
    same(3, proc_close($proc));
    unlink($app . '/index.php');
    rmdir($app);
}
