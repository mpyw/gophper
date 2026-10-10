<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Attributes\Fillable;
use Illuminate\Database\Eloquent\Model;

#[Fillable(['title', 'done'])]
class Todo extends Model
{
    protected function casts(): array
    {
        return [
            'done' => 'boolean',
        ];
    }
}
