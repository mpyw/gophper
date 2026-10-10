<?php

namespace App\Http\Controllers;

use App\Models\Todo;
use Illuminate\Http\RedirectResponse;
use Illuminate\Http\Request;
use Illuminate\View\View;

class TodoController extends Controller
{
    public function index(): View
    {
        return view('todos.index', [
            'todos' => Todo::query()->orderBy('done')->latest('id')->get(),
        ]);
    }

    public function store(Request $request): RedirectResponse
    {
        $validated = $request->validate([
            'title' => ['required', 'string', 'max:255'],
        ]);

        Todo::create($validated);

        return to_route('todos.index');
    }

    public function update(Todo $todo): RedirectResponse
    {
        $todo->update(['done' => ! $todo->done]);

        return to_route('todos.index');
    }

    public function destroy(Todo $todo): RedirectResponse
    {
        $todo->delete();

        return to_route('todos.index');
    }
}
