<?php

declare(strict_types=1);

namespace Hivepaas\Runtime;

use Hivepaas\Context;
use Hivepaas\Request;

/**
 * The PHP side of the runtime. The server sends calls on file descriptor 3 and
 * reads replies on 4: each message a line of JSON, then as many bytes of body as
 * the line says. The first reply says whether the handler was loaded. What the
 * function prints goes to its standard output and error, which the server
 * passes on; after a call, a line with SYNC and the call's number says that all
 * the call printed is out.
 */
final class Worker
{
    private const string SYNC = "\x1fhivepaas-sync:";
    private const int JSON_FLAGS = JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES | JSON_INVALID_UTF8_SUBSTITUTE;
    private const array FATAL = [E_ERROR, E_PARSE, E_CORE_ERROR, E_COMPILE_ERROR, E_USER_ERROR, E_RECOVERABLE_ERROR];

    private const int LOADING = 0;
    private const int IDLE = 1;
    private const int CALLING = 2;

    /** @var resource */
    private static $calls;
    /** @var resource */
    private static $replies;
    private static int $state = self::LOADING;
    private static int $seq = 0;
    private static int $maxBodySize = 6291456;
    private static ?\Closure $handler = null;

    /** Opens the server's pipes; from here on, a process that ends says why. */
    public static function begin(): void
    {
        $calls = fopen('php://fd/3', 'rb');
        $replies = fopen('php://fd/4', 'wb');
        if ($calls === false || $replies === false) {
            fwrite(STDERR, "hivepaas: the PHP process was started without the server's pipes\n");
            exit(2);
        }
        self::$calls = $calls;
        self::$replies = $replies;
        self::$maxBodySize = (int) (getenv('HP_FN_MAX_BODY_SIZE') ?: self::$maxBodySize);
        register_shutdown_function(self::shutdown(...));
    }

    /** Loads Composer's autoloader, when the function has libraries. The function is in the working directory. */
    public static function autoload(): void
    {
        $autoload = getcwd() . '/vendor/autoload.php';
        if (is_file($autoload)) {
            require_once $autoload;
        }
    }

    /** The file the handler is in. */
    public static function entrypoint(): string
    {
        $path = getcwd() . '/' . ltrim((string) getenv('HP_FN_ENTRYPOINT'), '/');
        if (!is_file($path)) {
            throw new \RuntimeException("the entrypoint {$path} is not a file");
        }
        return $path;
    }

    /** Says the handler could not be loaded, and why, and ends the process. */
    public static function notLoaded(\Throwable $e): never
    {
        self::$state = self::IDLE;
        self::reply(['loaded' => false, 'error' => (string) $e]);
        exit(3);
    }

    /** Says the handler is loaded, then runs the server's calls until it closes them. */
    public static function serve(): void
    {
        $name = (string) getenv('HP_FN_HANDLER');
        if (!is_callable($name)) {
            self::$state = self::IDLE;
            self::reply([
                'loaded' => false,
                'error' => sprintf('%s has no function %s', self::entrypoint(), $name),
            ]);
            exit(3);
        }
        self::$handler = \Closure::fromCallable($name);
        self::$state = self::IDLE;
        self::reply(['loaded' => true]);
        while (($line = fgets(self::$calls)) !== false) {
            /** @var array{seq: int, id: string, method: string, path: string, query: array<string, list<string>>,
             *             headers: array<string, list<string>>, deadline: float, bodyLength: int} $call */
            $call = json_decode($line, true, 512, JSON_THROW_ON_ERROR);
            self::run($call, $call['bodyLength'] > 0 ? self::read($call['bodyLength']) : '');
        }
    }

    /**
     * Runs one call, and replies.
     *
     * @param array{seq: int, id: string, method: string, path: string, query: array<string, list<string>>,
     *              headers: array<string, list<string>>, deadline: float, bodyLength: int} $call
     */
    private static function run(array $call, string $body): void
    {
        self::$seq = $call['seq'];
        $query = [];
        foreach ($call['query'] as $name => $values) {
            $query[$name] = $values[0] ?? '';
        }
        $headers = [];
        foreach ($call['headers'] as $name => $values) {
            $headers[$name] = implode(', ', $values);
        }
        $request = new Request($call['method'], $call['path'], $query, $call['query'], $headers, $body);
        $context = new Context($call['id'], (float) $call['deadline']);

        self::$state = self::CALLING;
        try {
            [$head, $answer] = Response::of((self::$handler)($request, $context), self::$maxBodySize);
        } catch (CallError $e) {
            [$head, $answer] = [['error' => $e->getMessage(), 'outcome' => $e->outcome], ''];
        } catch (\Throwable $e) {
            [$head, $answer] = [['error' => (string) $e], ''];
        }
        self::$state = self::IDLE;
        self::sync();
        self::reply(['seq' => self::$seq] + $head, $answer);
    }

    /** Reads exactly $length bytes of a call's body. */
    private static function read(int $length): string
    {
        $body = '';
        while (strlen($body) < $length) {
            $chunk = fread(self::$calls, $length - strlen($body));
            if ($chunk === false || $chunk === '') {
                throw new \RuntimeException('the server closed the call before its body');
            }
            $body .= $chunk;
        }
        return $body;
    }

    /** Says what the call printed is out: what a buffer kept goes out first. */
    private static function sync(): void
    {
        while (ob_get_level() > 0) {
            ob_end_flush();
        }
        // The function may have closed its standard output: then the server
        // stops waiting for this line.
        @fwrite(STDOUT, self::SYNC . self::$seq . "\n");
    }

    /**
     * Writes a reply and its body.
     *
     * @param array<string, mixed> $head
     */
    private static function reply(array $head, string $body = ''): void
    {
        if (array_key_exists('headers', $head) && $head['headers'] === []) {
            $head['headers'] = new \stdClass();
        }
        $head['bodyLength'] = strlen($body);
        $data = json_encode($head, self::JSON_FLAGS) . "\n" . $body;
        while ($data !== '') {
            $written = fwrite(self::$replies, $data);
            if ($written === false || $written === 0) {
                exit(1); // the server is gone
            }
            $data = substr($data, $written);
        }
        fflush(self::$replies);
    }

    /** A process that ends as it loads the handler, or in a call, says why. */
    private static function shutdown(): void
    {
        $error = error_get_last();
        $why = $error !== null && in_array($error['type'], self::FATAL, true)
            ? sprintf('PHP Fatal error: %s in %s:%d', $error['message'], $error['file'], $error['line'])
            : null;
        switch (self::$state) {
            case self::LOADING:
                self::$state = self::IDLE;
                self::reply(['loaded' => false, 'error' => $why ?? 'the function ended the process as it was loaded']);
                break;
            case self::CALLING:
                self::$state = self::IDLE;
                self::sync();
                self::reply(['seq' => self::$seq, 'error' => $why ?? 'the handler ended the process']);
                break;
        }
    }
}
