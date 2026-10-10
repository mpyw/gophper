<?php

namespace Tests\Feature;

use App\Models\Todo;
use Illuminate\Foundation\Testing\RefreshDatabase;
use Tests\TestCase;

class TodoTest extends TestCase
{
    use RefreshDatabase;

    public function test_it_adds_toggles_and_deletes_a_todo(): void
    {
        $this->get('/')->assertOk()->assertSee('Nothing to do.');

        $this->post('/todos', ['title' => 'Buy milk'])->assertRedirect('/');
        $todo = Todo::sole();
        $this->get('/')->assertSee('Buy milk')->assertSee('1 left');

        $this->patch("/todos/{$todo->id}")->assertRedirect('/');
        $this->assertTrue($todo->fresh()->done);

        $this->delete("/todos/{$todo->id}")->assertRedirect('/');
        $this->assertSame(0, Todo::count());
    }

    public function test_it_requires_a_title(): void
    {
        $this->post('/todos', ['title' => ''])->assertSessionHasErrors('title');
    }
}
