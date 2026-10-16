<?php

declare(strict_types=1);

namespace Hivepaas\Runtime;

/** What the handler returned cannot be answered: the call fails, with this outcome. */
final class CallError extends \Exception
{
    public function __construct(string $message, public readonly string $outcome = 'error')
    {
        parent::__construct($message);
    }
}
